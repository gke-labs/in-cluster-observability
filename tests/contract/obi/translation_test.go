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

package obi

import (
	"os"
	"path/filepath"
	"testing"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
)

// TestTranslationContract iterates every fixture under
// testdata/translation/ and runs it through the public translator
// (capture.TranslateMetrics / capture.TranslateTraces). The
// resulting capture.Event slice is normalized (timestamps zeroed)
// and diffed against the committed golden.json. Pass -update to
// regenerate goldens.
func TestTranslationContract(t *testing.T) {
	cases, err := filepath.Glob("testdata/translation/*")
	if err != nil {
		t.Fatalf("glob testdata/translation: %v", err)
	}
	if len(cases) == 0 {
		t.Skip("no fixtures in testdata/translation; run with -seed -update to bootstrap")
	}
	for _, dir := range cases {
		// Skip non-directories (e.g. RECORDED.md provenance, #151).
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			runTranslationCase(t, dir)
		})
	}
}

func runTranslationCase(t *testing.T, dir string) {
	kind, msg, goldenPath := loadCase(t, dir)

	var events []capture.Event
	switch kind {
	case "metrics":
		req := msg.(*collmetricspb.ExportMetricsServiceRequest)
		events = capture.TranslateMetrics(req.GetResourceMetrics())
	case "traces":
		req := msg.(*colltracepb.ExportTraceServiceRequest)
		events = capture.TranslateTraces(req.GetResourceSpans())
	default:
		t.Fatalf("unknown kind %q", kind)
	}

	compareOrUpdate(t, goldenPath, normalize(events))
}

func normalize(events []capture.Event) []goldenEvent {
	out := make([]goldenEvent, 0, len(events))
	for _, ev := range events {
		ge := goldenEvent{
			Kind:   eventKindString(ev.Kind),
			Module: ev.Module.String(),
		}
		if ev.Metric != nil {
			ge.Metric = &goldenMetric{
				Name:         ev.Metric.Name,
				Value:        ev.Metric.Value,
				Attributes:   ev.Metric.Attributes,
				Type:         metricTypeString(ev.Metric.Type),
				Temporality:  temporalityString(ev.Metric.Temporality),
				Monotonic:    ev.Metric.Monotonic,
				Count:        ev.Metric.Count,
				Bounds:       ev.Metric.Bounds,
				BucketCounts: ev.Metric.BucketCounts,
			}
		}
		if ev.Span != nil {
			ge.Span = &goldenSpan{
				Name:       ev.Span.Name,
				Method:     ev.Span.Method,
				Path:       ev.Span.Path,
				StatusCode: ev.Span.StatusCode,
				DurationNs: ev.Span.DurationNs,
				Attributes: ev.Span.Attributes,
			}
		}
		out = append(out, ge)
	}
	return out
}

func eventKindString(k capture.EventKind) string {
	switch k {
	case capture.EventMetric:
		return "metric"
	case capture.EventSpan:
		return "span"
	case capture.EventEdge:
		return "edge"
	case capture.EventModuleDegraded:
		return "module_degraded"
	default:
		return "unknown"
	}
}
