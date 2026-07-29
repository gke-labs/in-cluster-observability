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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
	"github.com/gke-labs/in-cluster-observability/pkg/schema"
)

// obiCollector re-exposes translated OBI MetricEvents on the agent's
// Prometheus registry as *const metrics* built from tracked state.
// This replaces the previous OTel-SDK re-recording, which could not be
// sound (#153): the SDK API records samples, so pre-aggregated OTLP
// data had to be guessed into counter/gauge shapes by metric-name
// suffix, cumulative totals were re-added every export interval, and
// histograms collapsed to their sum. The collector instead:
//
//   - honors OTLP temporality: cumulative monotonic sums pass their
//     running total straight through; delta sums accumulate here;
//     unspecified is treated as cumulative (the OTLP default).
//   - keeps histograms whole: count, sum, and explicit buckets are
//     re-emitted via ConstHistogram, so rate() and
//     histogram_quantile() work on :9090.
//   - uses the OTLP type system (Sum/Gauge/monotonic) instead of
//     name-suffix guessing.
//   - evicts series not reported for staleAfter, so pod churn can't
//     grow the scrape payload without bound (complements the #144
//     label allowlist).
type obiCollector struct {
	mu     sync.Mutex
	series map[string]*obiSeries
	// labelSchema pins the label-key set per metric name (first seen
	// wins); later points are coerced onto it. client_golang refuses
	// to scrape a name with inconsistent label dimensions, and one bad
	// series must not 500 the whole endpoint.
	labelSchema map[string][]string

	staleAfter time.Duration
	now        func() time.Time

	// droppedLabels counts attributes withheld from re-emission by
	// the schema.ForwardableLabel allowlist (#144), keyed by the
	// dropped attribute key so operators can see what's filtered.
	droppedLabels metric.Int64Counter
}

type seriesKind uint8

const (
	kindCounter seriesKind = iota
	kindGauge
	kindHistogram
)

type obiSeries struct {
	desc      *prometheus.Desc
	kind      seriesKind
	labelVals []string
	lastSeen  time.Time

	// counter running total or gauge last value
	value float64
	// histogram cumulative state
	count   uint64
	sum     float64
	bounds  []float64
	buckets []uint64
}

func newOBICollector(meter metric.Meter) *obiCollector {
	c := &obiCollector{
		series:      map[string]*obiSeries{},
		labelSchema: map[string][]string{},
		staleAfter:  10 * time.Minute,
		now:         time.Now,
	}
	c.droppedLabels, _ = meter.Int64Counter("ollie_forward_labels_dropped_total",
		metric.WithDescription("OBI attributes withheld from /metrics re-emission by the label allowlist, by attribute key"))
	return c
}

// Record folds one translated MetricEvent into the collector's state.
func (c *obiCollector) Record(ctx context.Context, ev capture.MetricEvent) {
	// OTel meta-metrics describe a Resource/Scope; OBI emits one per
	// discovered workload with help text that collides with any other
	// definition. Drop them — the agent's identity is ollie_agent_up.
	switch ev.Name {
	case "target_info", "otel_scope_info":
		return
	}

	name := promName(ev.Name)
	keys, vals, dropped := allowedLabels(ev.Attributes)
	for _, k := range dropped {
		if c.droppedLabels != nil {
			c.droppedLabels.Add(ctx, 1, metric.WithAttributes(attribute.String("label", k)))
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	schemaKeys, ok := c.labelSchema[name]
	if !ok {
		c.labelSchema[name] = keys
		schemaKeys = keys
	}
	vals = coerceToSchema(schemaKeys, keys, vals)

	key := name + "\x00" + strings.Join(vals, "\x00")
	st, ok := c.series[key]
	if !ok {
		st = &obiSeries{
			desc: prometheus.NewDesc(name,
				"re-emitted from the sibling OBI container (ollie agent, #153)",
				schemaKeys, nil),
			labelVals: vals,
		}
		c.series[key] = st
	}
	st.lastSeen = c.now()

	cumulative := ev.Temporality != capture.TemporalityDelta // unspecified ⇒ cumulative

	switch {
	case ev.Type == capture.MetricTypeHistogram:
		st.kind = kindHistogram
		if cumulative || !sameBounds(st.bounds, ev.Bounds) {
			// Cumulative snapshots replace state wholesale; a bounds
			// change (new OBI bucket layout) resets delta accumulation.
			st.count, st.sum = ev.Count, ev.Value
			st.bounds = append([]float64(nil), ev.Bounds...)
			st.buckets = append([]uint64(nil), ev.BucketCounts...)
		} else {
			st.count += ev.Count
			st.sum += ev.Value
			for i := range ev.BucketCounts {
				if i < len(st.buckets) {
					st.buckets[i] += ev.BucketCounts[i]
				}
			}
		}
	case ev.Type == capture.MetricTypeSum && ev.Monotonic:
		st.kind = kindCounter
		if cumulative {
			st.value = ev.Value // pass-through: OBI already keeps the total
		} else {
			st.value += ev.Value
		}
	default:
		// Gauges and non-monotonic sums: last value wins.
		st.kind = kindGauge
		st.value = ev.Value
	}
}

// Describe intentionally sends nothing, making this an "unchecked"
// collector: series come and go with workload churn and eviction, and
// registration-time descriptor checks would fight that.
func (c *obiCollector) Describe(chan<- *prometheus.Desc) {}

func (c *obiCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := c.now().Add(-c.staleAfter)
	for key, st := range c.series {
		if st.lastSeen.Before(cutoff) {
			delete(c.series, key)
			continue
		}
		switch st.kind {
		case kindCounter:
			m, err := prometheus.NewConstMetric(st.desc, prometheus.CounterValue, st.value, st.labelVals...)
			if err == nil {
				ch <- m
			}
		case kindGauge:
			m, err := prometheus.NewConstMetric(st.desc, prometheus.GaugeValue, st.value, st.labelVals...)
			if err == nil {
				ch <- m
			}
		case kindHistogram:
			// ConstHistogram wants cumulative per-le counts; OTLP
			// carries per-bucket counts with a trailing overflow
			// bucket that Prometheus represents implicitly via count.
			cum := uint64(0)
			le := make(map[float64]uint64, len(st.bounds))
			for i, b := range st.bounds {
				if i < len(st.buckets) {
					cum += st.buckets[i]
				}
				le[b] = cum
			}
			m, err := prometheus.NewConstHistogram(st.desc, st.count, st.sum, le, st.labelVals...)
			if err == nil {
				ch <- m
			}
		}
	}
}

// promName converts an OTel metric name to Prometheus form
// (http.server.request.duration → http_server_request_duration).
func promName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// allowedLabels filters attributes through the #144 allowlist and
// returns sorted, prom-safe label keys with matching values, plus the
// keys that were dropped.
func allowedLabels(attrs map[string]string) (keys, vals, dropped []string) {
	if len(attrs) == 0 {
		return nil, nil, nil
	}
	kept := make([]string, 0, len(attrs))
	for k := range attrs {
		if schema.ForwardableLabel(k) {
			kept = append(kept, k)
		} else {
			dropped = append(dropped, k)
		}
	}
	sort.Strings(kept)
	keys = make([]string, len(kept))
	vals = make([]string, len(kept))
	for i, k := range kept {
		keys[i] = promName(k)
		vals[i] = attrs[k]
	}
	return keys, vals, dropped
}

// coerceToSchema maps (keys, vals) onto the pinned schema key set:
// missing keys become "", extra keys are discarded. Guarantees every
// series of a name has identical label dimensions.
func coerceToSchema(schemaKeys, keys, vals []string) []string {
	if equalStrings(schemaKeys, keys) {
		return vals
	}
	out := make([]string, len(schemaKeys))
	for i, sk := range schemaKeys {
		for j, k := range keys {
			if k == sk {
				out[i] = vals[j]
				break
			}
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameBounds(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
