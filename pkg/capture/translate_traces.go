// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package capture

import (
	"strconv"
	"strings"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TranslateTraces walks an OTLP ResourceSpans tree and emits a
// capture.Event{Kind:Span} per Span. Spans are decoded with a minimal
// typed field set (method/path/status for HTTP, method/status for
// gRPC, plus duration_ns) and the raw attribute map. Per ADR-0021,
// OBI's k8s.* / service.* attrs flow through unchanged.
//
// The event Module discriminates the protocol. gRPC rides HTTP/2 but
// OBI attributes it distinctly (rpc.* attributes, own span name), so
// we classify a span as ModuleGRPC when it carries the gRPC RPC
// attributes and ModuleHTTP1 otherwise. OBI v0.10.0 does not label
// HTTP protocol version, so plaintext HTTP/1.1 and h2c both classify
// as ModuleHTTP1 — they are indistinguishable at this layer (ADR-0031).
//
// OBI's exact attribute keys vary by semconv version. For HTTP we
// accept both the current `http.request.method` / `url.path` /
// `http.response.status_code` shape and the older `http.method` /
// `http.url` / `http.status_code` shape. For gRPC we read the semconv
// v1.41.0 keys OBI pins (`rpc.method`, `rpc.response.status_code`)
// with older-form fallbacks.
func TranslateTraces(rs []*tracepb.ResourceSpans) []Event {
	var out []Event
	now := time.Now()
	for _, r := range rs {
		resAttrs := kvToMap(r.GetResource().GetAttributes())
		for _, ss := range r.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				attrs := mergeMaps(resAttrs, kvToMap(sp.GetAttributes()))
				se := &SpanEvent{
					Name:       sp.GetName(),
					DurationNs: spanDurationNs(sp),
					Attributes: attrs,
				}
				module := ModuleHTTP1
				if isGRPCSpan(attrs) {
					module = ModuleGRPC
					se.RPCMethod = pickFirst(attrs, "rpc.method")
					se.RPCStatus = pickFirst(attrs, "rpc.response.status_code", "rpc.grpc.status_code")
				} else {
					se.Method = pickFirst(attrs, "http.request.method", "http.method")
					se.Path = pickFirst(attrs, "url.path", "http.url", "http.target")
					se.StatusCode = parseStatus(pickFirst(attrs, "http.response.status_code", "http.status_code"))
				}
				out = append(out, Event{
					Kind:      EventSpan,
					Timestamp: now,
					Module:    module,
					Span:      se,
				})
			}
		}
	}
	return out
}

// isGRPCSpan reports whether an OTLP span carries OBI's gRPC RPC
// attributes. OBI v0.10.0 sets rpc.system.name="grpc" (semconv
// v1.41.0); we also accept the older rpc.system key and, as a last
// resort, the presence of rpc.method — OBI only emits rpc.* for gRPC,
// so any RPC attribute is a reliable discriminator.
func isGRPCSpan(attrs map[string]string) bool {
	if attrs["rpc.system.name"] == "grpc" || attrs["rpc.system"] == "grpc" {
		return true
	}
	return attrs["rpc.method"] != ""
}

// pickFirst returns the first non-empty value across keys; "" if none.
func pickFirst(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// parseStatus turns an HTTP status string into an int; 0 if empty/bad.
func parseStatus(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// spanDurationNs returns (end - start) in nanoseconds; 0 if either
// timestamp is unset or end <= start.
func spanDurationNs(sp *tracepb.Span) uint64 {
	start := sp.GetStartTimeUnixNano()
	end := sp.GetEndTimeUnixNano()
	if start == 0 || end == 0 || end <= start {
		return 0
	}
	return end - start
}
