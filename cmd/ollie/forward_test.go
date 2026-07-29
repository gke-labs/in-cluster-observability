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
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
)

func newTestCollector() *obiCollector {
	return newOBICollector(noop.NewMeterProvider().Meter("test"))
}

// render gathers the collector through a fresh registry and returns
// the text exposition.
func render(t *testing.T, c *obiCollector) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	got, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var b strings.Builder
	for _, mf := range got {
		b.WriteString(mf.String())
		b.WriteString("\n")
	}
	return b.String()
}

// Cumulative monotonic sums must pass through, not re-add — the #153
// counter-inflation bug. OBI reports the running total every export
// interval; two reports of a growing total must expose the total.
func TestCollector_CumulativeSumPassesThrough(t *testing.T) {
	c := newTestCollector()
	ev := capture.MetricEvent{
		Name: "tcp.rx.bytes", Type: capture.MetricTypeSum,
		Temporality: capture.TemporalityCumulative, Monotonic: true,
	}
	ev.Value = 100
	c.Record(context.Background(), ev)
	ev.Value = 250
	c.Record(context.Background(), ev)

	if got := testutil.ToFloat64(c); got != 250 {
		t.Fatalf("cumulative sum must pass through: got %v, want 250 (not 350)", got)
	}
}

// Unspecified temporality must be treated as cumulative (OTLP default)
// — the safe direction: worst case is a stuck counter, never an
// inflated one.
func TestCollector_UnspecifiedTemporalityIsCumulative(t *testing.T) {
	c := newTestCollector()
	ev := capture.MetricEvent{
		Name: "tcp.tx.bytes", Type: capture.MetricTypeSum,
		Temporality: capture.TemporalityUnspecified, Monotonic: true,
	}
	ev.Value = 10
	c.Record(context.Background(), ev)
	c.Record(context.Background(), ev)

	if got := testutil.ToFloat64(c); got != 10 {
		t.Fatalf("unspecified temporality must not accumulate: got %v, want 10", got)
	}
}

// Delta sums accumulate here.
func TestCollector_DeltaSumAccumulates(t *testing.T) {
	c := newTestCollector()
	ev := capture.MetricEvent{
		Name: "http.requests", Type: capture.MetricTypeSum,
		Temporality: capture.TemporalityDelta, Monotonic: true,
	}
	ev.Value = 3
	c.Record(context.Background(), ev)
	ev.Value = 4
	c.Record(context.Background(), ev)

	if got := testutil.ToFloat64(c); got != 7 {
		t.Fatalf("delta sum must accumulate: got %v, want 7", got)
	}
}

// Histograms keep count, sum, and buckets — rate() and
// histogram_quantile() must work downstream. Delta points merge.
func TestCollector_HistogramBucketsPreserved(t *testing.T) {
	c := newTestCollector()
	ev := capture.MetricEvent{
		Name: "http.server.request.duration", Type: capture.MetricTypeHistogram,
		Temporality: capture.TemporalityDelta,
		Count:       3, Value: 0.6,
		Bounds:       []float64{0.1, 1},
		BucketCounts: []uint64{2, 1, 0},
	}
	c.Record(context.Background(), ev)
	c.Record(context.Background(), ev) // second delta report merges

	out := render(t, c)
	for _, want := range []string{
		`sample_count:6`,
		`upper_bound:0.1`,
		`cumulative_count:4`,
		`cumulative_count:6`,
	} {
		if !strings.Contains(strings.ReplaceAll(out, " ", ""), want) {
			t.Errorf("missing %q in rendered histogram:\n%s", want, out)
		}
	}
}

// Cumulative histogram snapshots replace state wholesale.
func TestCollector_HistogramCumulativeSnapshot(t *testing.T) {
	c := newTestCollector()
	ev := capture.MetricEvent{
		Name: "http.server.request.duration", Type: capture.MetricTypeHistogram,
		Temporality: capture.TemporalityCumulative,
		Count:       5, Value: 1.5,
		Bounds:       []float64{0.5},
		BucketCounts: []uint64{4, 1},
	}
	c.Record(context.Background(), ev)
	ev.Count, ev.Value, ev.BucketCounts = 9, 2.5, []uint64{7, 2}
	c.Record(context.Background(), ev)

	out := strings.ReplaceAll(render(t, c), " ", "")
	if !strings.Contains(out, "sample_count:9") {
		t.Fatalf("cumulative histogram must snapshot to count 9:\n%s", out)
	}
}

// The old name-suffix heuristic classified any *.duration as a
// counter; a gauge-typed point named that way must stay a gauge now.
func TestCollector_TypeComesFromOTLPNotName(t *testing.T) {
	c := newTestCollector()
	ev := capture.MetricEvent{Name: "queue.wait.duration", Type: capture.MetricTypeGauge}
	ev.Value = 5
	c.Record(context.Background(), ev)
	ev.Value = 2
	c.Record(context.Background(), ev)

	out := render(t, c)
	if !strings.Contains(out, "type:GAUGE") && !strings.Contains(out, "GAUGE") {
		t.Fatalf("gauge-typed point must render as gauge:\n%s", out)
	}
	if got := testutil.ToFloat64(c); got != 2 {
		t.Fatalf("gauge must hold last value: got %v, want 2", got)
	}
}

// Series unseen for staleAfter are evicted so pod churn can't grow
// the scrape payload without bound.
func TestCollector_StaleSeriesEvicted(t *testing.T) {
	c := newTestCollector()
	fake := time.Now()
	c.now = func() time.Time { return fake }

	c.Record(context.Background(), capture.MetricEvent{
		Name: "tcp.rx.bytes", Type: capture.MetricTypeSum,
		Temporality: capture.TemporalityCumulative, Monotonic: true, Value: 1,
		Attributes: map[string]string{"k8s.pod.name": "gone-pod"},
	})
	if n := testutil.CollectAndCount(c); n != 1 {
		t.Fatalf("series count = %d, want 1", n)
	}
	fake = fake.Add(11 * time.Minute)
	if n := testutil.CollectAndCount(c); n != 0 {
		t.Fatalf("stale series not evicted; count = %d, want 0", n)
	}
}

// Label dimensions stay consistent per metric name even when points
// disagree (allowlist churn, OBI version skew): one odd series must
// not 500 the whole scrape.
func TestCollector_LabelSchemaCoerced(t *testing.T) {
	c := newTestCollector()
	c.Record(context.Background(), capture.MetricEvent{
		Name: "tcp.rx.bytes", Type: capture.MetricTypeSum,
		Temporality: capture.TemporalityCumulative, Monotonic: true, Value: 1,
		Attributes: map[string]string{"k8s.pod.name": "a", "k8s.namespace.name": "ns"},
	})
	c.Record(context.Background(), capture.MetricEvent{
		Name: "tcp.rx.bytes", Type: capture.MetricTypeSum,
		Temporality: capture.TemporalityCumulative, Monotonic: true, Value: 2,
		Attributes: map[string]string{"k8s.pod.name": "b"}, // missing namespace
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather must not fail on differing label sets: %v", err)
	}
	if n := testutil.CollectAndCount(c); n != 2 {
		t.Fatalf("series count = %d, want 2", n)
	}
}

// Allowlist behavior carried over from the pre-#153 forwarder (#144):
// only schema.ForwardableLabel keys are re-emitted, and drops are
// accounted on the self-obs counter.
func TestCollector_LabelAllowlistAndDropAccounting(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	c := newOBICollector(mp.Meter("test"))

	c.Record(ctx, capture.MetricEvent{
		Name: "http.server.request.duration.sum.like", Type: capture.MetricTypeSum,
		Temporality: capture.TemporalityCumulative, Monotonic: true, Value: 1,
		Attributes: map[string]string{
			"k8s.pod.name": "nginx-abc",
			"url.path":     "/users/42?token=hunter2",
		},
	})

	out := render(t, c)
	if strings.Contains(out, "url_path") || strings.Contains(out, "hunter2") {
		t.Fatalf("non-allowlisted label leaked:\n%s", out)
	}
	if !strings.Contains(out, `k8s_pod_name`) {
		t.Fatalf("allowlisted label missing:\n%s", out)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ollie_forward_labels_dropped_total" {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	if total != 1 {
		t.Fatalf("dropped-label count = %d, want 1", total)
	}
}

// target_info / otel_scope_info stay dropped.
func TestCollector_MetaMetricsDropped(t *testing.T) {
	c := newTestCollector()
	c.Record(context.Background(), capture.MetricEvent{Name: "target_info", Type: capture.MetricTypeGauge, Value: 1})
	c.Record(context.Background(), capture.MetricEvent{Name: "otel_scope_info", Type: capture.MetricTypeGauge, Value: 1})
	if n := testutil.CollectAndCount(c); n != 0 {
		t.Fatalf("meta-metrics must be dropped; got %d series", n)
	}
}
