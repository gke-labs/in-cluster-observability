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
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/gke-labs/in-cluster-observability/internal/store"
)

// agentRawTee fans the bridge's raw OTLP payloads out to their
// consumers: traces feed the span ring (#84); metrics feed the
// export relays once those land (#97/#98). All paths are
// non-blocking per the capture.RawTee contract.
type agentRawTee struct {
	spans *store.SpanBuffer
}

func (t *agentRawTee) RawMetrics(_ *collmetricspb.ExportMetricsServiceRequest) {
	// Export relay consumer lands in the egress phase (#97/#98).
}

func (t *agentRawTee) RawTraces(req *colltracepb.ExportTraceServiceRequest) {
	if t.spans != nil {
		t.spans.AppendRequest(req)
	}
}
