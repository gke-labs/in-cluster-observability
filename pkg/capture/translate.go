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
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// TranslateMetrics walks an OTLP ResourceMetrics tree and emits a
// capture.Event per datapoint. Resource attributes are merged into
// each event's Attributes map. Per ADR-0021 OBI is the source of K8s
// identity; the k8s.* / service.* attrs OBI attaches flow through
// unchanged so re-emitted Prometheus metrics carry them.
//
// Names are passed through unchanged (no `ollie_*` rewrite — also
// per ADR-0021).
func TranslateMetrics(rms []*metricspb.ResourceMetrics) []Event {
	var out []Event
	now := time.Now()
	for _, rm := range rms {
		resAttrs := kvToMap(rm.GetResource().GetAttributes())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				module := classifyMetric(m.GetName())
				for _, dp := range datapointsOf(m) {
					attrs := mergeMaps(resAttrs, kvToMap(dp.attrs))
					out = append(out, Event{
						Kind:      EventMetric,
						Timestamp: now,
						Module:    module,
						Metric: &MetricEvent{
							Name:         m.GetName(),
							Value:        dp.value,
							Attributes:   attrs,
							Type:         dp.typ,
							Temporality:  dp.temporality,
							Monotonic:    dp.monotonic,
							Count:        dp.count,
							Bounds:       dp.bounds,
							BucketCounts: dp.bucketCounts,
						},
					})
				}
			}
		}
	}
	return out
}

// classifyMetric maps an OBI metric name to a capture.Module. The
// mapping is heuristic — OBI's metric naming follows OTel semconv
// (which the contract tests in #74 will validate against real OBI
// output). Unknown names fall through to ModuleL4TCP since that's
// the metric-only module in v0.2; HTTP metrics also classify here.
func classifyMetric(name string) Module {
	switch {
	case strings.HasPrefix(name, "http."), strings.HasPrefix(name, "http_"),
		strings.Contains(name, ".http."), strings.Contains(name, "_http_"):
		return ModuleHTTP1
	default:
		return ModuleL4TCP
	}
}

// dp is the union shape for OTLP number datapoints. We flatten Sum,
// Gauge, and Histogram datapoints into this so the translator stays
// linear regardless of metric type. Type, temporality, monotonicity,
// and histogram buckets are carried through so consumers can re-emit
// soundly (#153).
type dp struct {
	value float64
	attrs []*commonpb.KeyValue

	typ         MetricType
	temporality Temporality
	monotonic   bool

	count        uint64
	bounds       []float64
	bucketCounts []uint64
}

func temporalityOf(t metricspb.AggregationTemporality) Temporality {
	switch t {
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
		return TemporalityDelta
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
		return TemporalityCumulative
	default:
		return TemporalityUnspecified
	}
}

func datapointsOf(m *metricspb.Metric) []dp {
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		temp := temporalityOf(d.Sum.GetAggregationTemporality())
		mono := d.Sum.GetIsMonotonic()
		out := make([]dp, 0, len(d.Sum.GetDataPoints()))
		for _, p := range d.Sum.GetDataPoints() {
			out = append(out, dp{
				value: numberValue(p), attrs: p.GetAttributes(),
				typ: MetricTypeSum, temporality: temp, monotonic: mono,
			})
		}
		return out
	case *metricspb.Metric_Gauge:
		out := make([]dp, 0, len(d.Gauge.GetDataPoints()))
		for _, p := range d.Gauge.GetDataPoints() {
			out = append(out, dp{
				value: numberValue(p), attrs: p.GetAttributes(),
				typ: MetricTypeGauge,
			})
		}
		return out
	case *metricspb.Metric_Histogram:
		temp := temporalityOf(d.Histogram.GetAggregationTemporality())
		out := make([]dp, 0, len(d.Histogram.GetDataPoints()))
		for _, p := range d.Histogram.GetDataPoints() {
			// Value carries the sum; count + explicit buckets ride
			// alongside so downstream can rebuild the full histogram.
			out = append(out, dp{
				value: p.GetSum(), attrs: p.GetAttributes(),
				typ: MetricTypeHistogram, temporality: temp,
				count:        p.GetCount(),
				bounds:       p.GetExplicitBounds(),
				bucketCounts: p.GetBucketCounts(),
			})
		}
		return out
	}
	return nil
}

func numberValue(p *metricspb.NumberDataPoint) float64 {
	switch v := p.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	}
	return 0
}

// kvToMap flattens a slice of OTLP common KeyValue pairs into a
// string-keyed string-valued map. Non-string values are stringified.
func kvToMap(kvs []*commonpb.KeyValue) map[string]string {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = anyValueString(kv.GetValue())
	}
	return m
}

func anyValueString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return formatInt(x.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return formatFloat(x.DoubleValue)
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	}
	return ""
}

// formatInt / formatFloat without strconv allocations on the hot path.
// Kept tiny — the translator runs per OTLP message, not per attribute
// in deep loops, so simplicity wins.
func formatInt(n int64) string {
	// Tiny helper; package-level allocations are fine.
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatFloat(f float64) string {
	// Use a default 'g' rendering for compactness; for self-obs metric
	// attributes this is fine. We avoid importing strconv just to keep
	// the translator's import set tight; this is the only float path.
	if f == 0 {
		return "0"
	}
	// Fall back to the runtime's formatting via interfaces.
	type stringer interface{ String() string }
	_ = stringer(nil) // unused; keep for documentation
	return floatToStr(f)
}

// floatToStr is a minimal float formatter. Uses the standard fmt
// package internally via the indirect route below to avoid pulling
// strconv into this file unconditionally. For v0.2 self-obs use,
// 6 significant digits are plenty.
func floatToStr(f float64) string { return fmtSprintf("%g", f) }

// fmtSprintf is a tiny local shim so the imports list stays explicit
// about what we use. We don't actually need fmt at runtime — Sprintf
// is just an indirection. Implemented via the standard library.
var fmtSprintf = func(format string, args ...any) string {
	return sprintf(format, args...)
}

// sprintf delegates to the standard fmt.Sprintf via a one-line wrapper
// declared in translate_fmt.go to keep this file's imports focused on
// OTLP types.
