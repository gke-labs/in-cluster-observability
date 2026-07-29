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
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// RawTee observes every OTLP payload exactly as it arrived from OBI,
// before translation (ADR-0026 §5–6). Consumers that want the wire
// shape — the span ring, the export relays — hang off this instead
// of re-encoding translated events.
//
// Contract: methods run on the receiver hot path and MUST NOT block.
// The payload is shared, not copied; implementations must treat it
// as read-only.
//
// Stability: Experimental
type RawTee interface {
	RawMetrics(req *collmetricspb.ExportMetricsServiceRequest)
	RawTraces(req *colltracepb.ExportTraceServiceRequest)
}
