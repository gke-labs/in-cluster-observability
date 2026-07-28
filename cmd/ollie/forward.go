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
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
	"github.com/gke-labs/in-cluster-observability/pkg/schema"
)

// metricForwarder re-records translated MetricEvents into an OTel SDK
// Meter so the same MeterProvider's Prometheus exporter exposes them
// (alongside the agent's own self-observability counters). This is
// what makes "single scrape URL through the agent" real per ADR-0021.
//
// The forwarder is intentionally simple: instruments are cached by
// name, and the cumulative-vs-gauge decision is a name-suffix
// heuristic. Histograms collapse to their sum (the translator already
// did the bucket loss). v0.5's store will get proper aggregation.
type metricForwarder struct {
	meter    metric.Meter
	counters sync.Map // map[string]metric.Float64Counter
	gauges   sync.Map // map[string]metric.Float64Gauge

	// droppedLabels counts attributes withheld from re-emission by
	// the schema.ForwardableLabel allowlist (#144), keyed by the
	// dropped attribute key so operators can see what's filtered.
	droppedLabels metric.Int64Counter
}

func newMetricForwarder(meter metric.Meter) *metricForwarder {
	f := &metricForwarder{meter: meter}
	f.droppedLabels, _ = meter.Int64Counter("ollie_forward_labels_dropped_total",
		metric.WithDescription("OBI attributes withheld from /metrics re-emission by the label allowlist, by attribute key"))
	return f
}

func (f *metricForwarder) Record(ctx context.Context, ev capture.MetricEvent) {
	// OTel SDK meta-metrics describe a Resource/Scope and are emitted
	// by the Prometheus exporter from the *agent's* MeterProvider
	// Resource. Re-recording OBI's per-workload copies through our
	// Meter would produce duplicate definitions (different help
	// strings), which client_golang refuses to scrape — the whole
	// /metrics endpoint returns an error in that case. Drop them.
	switch ev.Name {
	case "target_info", "otel_scope_info":
		return
	}
	kvs, dropped := attrsToOTel(ev.Attributes)
	for _, key := range dropped {
		if f.droppedLabels != nil {
			f.droppedLabels.Add(ctx, 1, metric.WithAttributes(attribute.String("label", key)))
		}
	}
	opts := metric.WithAttributes(kvs...)
	if looksCumulative(ev.Name) {
		if c := f.counter(ev.Name); c != nil {
			c.Add(ctx, ev.Value, opts)
		}
	} else {
		if g := f.gauge(ev.Name); g != nil {
			g.Record(ctx, ev.Value, opts)
		}
	}
}

func (f *metricForwarder) counter(name string) metric.Float64Counter {
	if v, ok := f.counters.Load(name); ok {
		return v.(metric.Float64Counter)
	}
	c, err := f.meter.Float64Counter(name)
	if err != nil {
		return nil
	}
	actual, _ := f.counters.LoadOrStore(name, c)
	return actual.(metric.Float64Counter)
}

func (f *metricForwarder) gauge(name string) metric.Float64Gauge {
	if v, ok := f.gauges.Load(name); ok {
		return v.(metric.Float64Gauge)
	}
	g, err := f.meter.Float64Gauge(name)
	if err != nil {
		return nil
	}
	actual, _ := f.gauges.LoadOrStore(name, g)
	return actual.(metric.Float64Gauge)
}

// looksCumulative classifies a metric name as counter-style (Add) vs
// gauge-style (Record). OBI's metric names follow OTel semconv —
// counters end in suffixes like .total / .count / .bytes / .duration
// / .size; gauges (e.g. tcp.rtt, *.queue.length) don't. The heuristic
// is intentionally permissive: false positives accumulate a sane
// counter where a gauge would be slightly better, which is acceptable
// for v0.3 smoke testing.
func looksCumulative(name string) bool {
	for _, suf := range cumulativeSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

var cumulativeSuffixes = []string{
	"_total", ".total",
	"_count", ".count",
	"_bytes", ".bytes",
	"_seconds", ".seconds",
	"_requests", ".requests",
	"_duration", ".duration",
	"_size", ".size",
	".rx", ".tx",
}

// attrsToOTel converts MetricEvent attributes to OTel KeyValues,
// admitting only keys on the schema.ForwardableLabel allowlist. The
// scrape endpoint's payload is bounded by that allowlist rather than
// by whatever the pinned OBI version emits (#144). Dropped keys are
// returned for self-obs accounting.
func attrsToOTel(m map[string]string) (kvs []attribute.KeyValue, dropped []string) {
	if len(m) == 0 {
		return nil, nil
	}
	kvs = make([]attribute.KeyValue, 0, len(m))
	for k, v := range m {
		if !schema.ForwardableLabel(k) {
			dropped = append(dropped, k)
			continue
		}
		kvs = append(kvs, attribute.String(k, v))
	}
	return kvs, dropped
}
