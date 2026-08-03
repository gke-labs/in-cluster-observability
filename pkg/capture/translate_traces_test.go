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
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func httpSpan(name, method, path, status string, startNs, endNs uint64) *tracepb.Span {
	return &tracepb.Span{
		Name:              name,
		StartTimeUnixNano: startNs,
		EndTimeUnixNano:   endNs,
		Attributes: []*commonpb.KeyValue{
			strKV("http.request.method", method),
			strKV("url.path", path),
			strKV("http.response.status_code", status),
		},
	}
}

func TestTranslateTraces_HTTP11(t *testing.T) {
	rs := []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strKV("k8s.pod.name", "passes-through"),
				strKV("custom.tag", "keep-me"),
			},
		},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{
				httpSpan("GET /users/42", "GET", "/users/42", "200",
					/*start*/ 1_000_000_000 /*end*/, 1_012_000_000),
			},
		}},
	}}

	events := TranslateTraces(rs)
	if len(events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != EventSpan {
		t.Errorf("kind = %v; want span", ev.Kind)
	}
	if ev.Module != ModuleHTTP1 {
		t.Errorf("module = %v; want http1", ev.Module)
	}
	if ev.Span == nil {
		t.Fatal("Span payload nil")
	}
	if ev.Span.Method != "GET" {
		t.Errorf("Method = %q; want GET", ev.Span.Method)
	}
	if ev.Span.Path != "/users/42" {
		t.Errorf("Path = %q; want /users/42", ev.Span.Path)
	}
	if ev.Span.StatusCode != 200 {
		t.Errorf("StatusCode = %d; want 200", ev.Span.StatusCode)
	}
	if ev.Span.DurationNs != 12_000_000 {
		t.Errorf("DurationNs = %d; want 12_000_000", ev.Span.DurationNs)
	}
	if got := ev.Span.Attributes["k8s.pod.name"]; got != "passes-through" {
		t.Errorf("k8s.pod.name should pass through (ADR-0021); got %q in %v", got, ev.Span.Attributes)
	}
	if got := ev.Span.Attributes["custom.tag"]; got != "keep-me" {
		t.Errorf("custom.tag = %q; want keep-me", got)
	}
}

func TestTranslateTraces_LegacySemconv(t *testing.T) {
	// OBI image may emit either current or legacy HTTP semconv. Test
	// the legacy attribute keys still resolve.
	rs := []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{
				Name:              "POST /api",
				StartTimeUnixNano: 100,
				EndTimeUnixNano:   200,
				Attributes: []*commonpb.KeyValue{
					strKV("http.method", "POST"),
					strKV("http.url", "https://x/api"),
					strKV("http.status_code", "201"),
				},
			}},
		}},
	}}
	events := TranslateTraces(rs)
	if len(events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(events))
	}
	se := events[0].Span
	if se.Method != "POST" || se.StatusCode != 201 {
		t.Errorf("legacy semconv not picked up: %+v", se)
	}
	if se.Path != "https://x/api" {
		t.Errorf("Path fallback to http.url failed: %q", se.Path)
	}
}

func TestTranslateTraces_GRPC(t *testing.T) {
	// OBI attributes gRPC distinctly from plaintext HTTP/2: the span
	// carries rpc.* (semconv v1.41.0) and the span name is the full
	// method path. It must classify as ModuleGRPC with the RPC fields
	// promoted and the HTTP fields left empty (ADR-0031).
	rs := []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{strKV("k8s.pod.name", "echo-server")},
		},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{
				Name:              "/grpc.health.v1.Health/Check",
				StartTimeUnixNano: 1_000_000_000,
				EndTimeUnixNano:   1_003_000_000,
				Attributes: []*commonpb.KeyValue{
					strKV("rpc.system.name", "grpc"),
					strKV("rpc.method", "/grpc.health.v1.Health/Check"),
					strKV("rpc.response.status_code", "0"),
				},
			}},
		}},
	}}
	events := TranslateTraces(rs)
	if len(events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(events))
	}
	ev := events[0]
	if ev.Module != ModuleGRPC {
		t.Errorf("module = %v; want grpc", ev.Module)
	}
	se := ev.Span
	if se.RPCMethod != "/grpc.health.v1.Health/Check" {
		t.Errorf("RPCMethod = %q; want the full gRPC path", se.RPCMethod)
	}
	if se.RPCStatus != "0" {
		t.Errorf("RPCStatus = %q; want 0", se.RPCStatus)
	}
	// HTTP-shaped fields must stay empty for gRPC.
	if se.Method != "" || se.Path != "" || se.StatusCode != 0 {
		t.Errorf("HTTP fields should be empty for gRPC; got Method=%q Path=%q Status=%d", se.Method, se.Path, se.StatusCode)
	}
	if se.DurationNs != 3_000_000 {
		t.Errorf("DurationNs = %d; want 3_000_000", se.DurationNs)
	}
	if got := se.Attributes["k8s.pod.name"]; got != "echo-server" {
		t.Errorf("k8s.pod.name should pass through; got %q", got)
	}
}

func TestTranslateTraces_GRPCLegacyAndFallback(t *testing.T) {
	// Older semconv uses rpc.system (not rpc.system.name) and
	// rpc.grpc.status_code; and some OBI builds may set only rpc.method.
	// Both must still classify as gRPC.
	cases := []struct {
		name       string
		attrs      []*commonpb.KeyValue
		wantStatus string
	}{
		{
			name: "legacy-rpc-system",
			attrs: []*commonpb.KeyValue{
				strKV("rpc.system", "grpc"),
				strKV("rpc.method", "/svc/M"),
				strKV("rpc.grpc.status_code", "5"),
			},
			wantStatus: "5",
		},
		{
			name: "method-only",
			attrs: []*commonpb.KeyValue{
				strKV("rpc.method", "/svc/M"),
			},
			wantStatus: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := []*tracepb.ResourceSpans{{
				ScopeSpans: []*tracepb.ScopeSpans{{
					Spans: []*tracepb.Span{{Name: "/svc/M", Attributes: tc.attrs}},
				}},
			}}
			events := TranslateTraces(rs)
			if len(events) != 1 {
				t.Fatalf("expected 1 event; got %d", len(events))
			}
			if events[0].Module != ModuleGRPC {
				t.Errorf("module = %v; want grpc", events[0].Module)
			}
			if got := events[0].Span.RPCStatus; got != tc.wantStatus {
				t.Errorf("RPCStatus = %q; want %q", got, tc.wantStatus)
			}
		})
	}
}

func TestTranslateTraces_NoDuration(t *testing.T) {
	rs := []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{
			Spans: []*tracepb.Span{{Name: "no-time"}}, // both timestamps zero
		}},
	}}
	events := TranslateTraces(rs)
	if len(events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(events))
	}
	if events[0].Span.DurationNs != 0 {
		t.Errorf("DurationNs = %d; want 0 when timestamps missing", events[0].Span.DurationNs)
	}
}

func TestTranslateTraces_EmptyInput(t *testing.T) {
	if got := TranslateTraces(nil); len(got) != 0 {
		t.Errorf("nil input should produce no events; got %d", len(got))
	}
}
