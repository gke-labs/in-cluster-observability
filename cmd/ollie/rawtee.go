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

package main

import (
	"strings"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/gke-labs/in-cluster-observability/internal/export"
	"github.com/gke-labs/in-cluster-observability/internal/store"
)

// agentRawTee fans the bridge's raw OTLP payloads out to their
// consumers: traces feed the span ring (#84); metrics feed the
// export relays once those land (#97/#98). All paths are
// non-blocking per the capture.RawTee contract.
type agentRawTee struct {
	spans *store.SpanBuffer
	otlp  *export.OTLPRelays
}

func (t *agentRawTee) RawMetrics(req *collmetricspb.ExportMetricsServiceRequest) {
	if t.otlp != nil {
		t.otlp.Metrics.Enqueue(req)
	}
}

func (t *agentRawTee) RawTraces(req *colltracepb.ExportTraceServiceRequest) {
	if t.spans != nil {
		t.spans.AppendRequest(req)
	}
	if t.otlp != nil {
		t.otlp.Traces.Enqueue(req)
	}
}

// parseHeaders parses "k=v,k2=v2" flag syntax.
func parseHeaders(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
