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

package store

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/prometheus/model/labels"
)

// Ingester feeds the store by periodically gathering a Prometheus
// registry and appending every sample (ADR-0025 §2: the same registry
// backs the :9090 scrape endpoint, so what PromQL sees is exactly
// what a scraper would see — one normalization path).
type Ingester struct {
	store    *Store
	gatherer prometheus.Gatherer
	interval time.Duration
	logger   *slog.Logger

	samplesAppended prometheus.Counter
	appendErrors    prometheus.Counter
	activeSeries    prometheus.GaugeFunc
}

// NewIngester wires an ingester gathering from g every interval
// (default 1s). Self-observability metrics are registered on reg,
// which is typically the same registry g gathers — the ollie_store_*
// series then flow into the store like everything else.
func NewIngester(s *Store, g prometheus.Gatherer, reg prometheus.Registerer, interval time.Duration, logger *slog.Logger) *Ingester {
	if interval <= 0 {
		interval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	ing := &Ingester{
		store:    s,
		gatherer: g,
		interval: interval,
		logger:   logger,
		samplesAppended: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ollie_store_samples_appended_total",
			Help: "Samples appended to the node-local tsdb.",
		}),
		appendErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ollie_store_append_errors_total",
			Help: "Samples that failed to append to the node-local tsdb.",
		}),
		activeSeries: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ollie_store_active_series",
			Help: "Series currently active in the tsdb head block.",
		}, func() float64 { return float64(s.NumActiveSeries()) }),
	}
	if reg != nil {
		reg.MustRegister(ing.samplesAppended, ing.appendErrors, ing.activeSeries)
	}
	return ing
}

// Run ticks until ctx is done. Blocking; run in a goroutine.
func (ing *Ingester) Run(ctx context.Context) {
	t := time.NewTicker(ing.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ing.Tick(ctx)
		}
	}
}

// Tick gathers once and appends every sample at the current
// timestamp. Exported for tests.
func (ing *Ingester) Tick(ctx context.Context) {
	fams, err := ing.gatherer.Gather()
	if err != nil {
		// Gather returns partial results with a MultiError; keep
		// what we got and note the rest.
		ing.logger.Warn("store ingest: partial gather", "err", err)
	}

	ts := time.Now().UnixMilli()
	app := ing.store.Appender(ctx)
	var appended, failed int
	for _, fam := range fams {
		for _, m := range fam.GetMetric() {
			for _, smp := range flatten(fam, m) {
				lb := labels.NewScratchBuilder(len(m.GetLabel()) + 2)
				lb.Add(labels.MetricName, smp.name)
				for _, lp := range m.GetLabel() {
					lb.Add(lp.GetName(), lp.GetValue())
				}
				if smp.le != "" {
					lb.Add(labels.BucketLabel, smp.le)
				}
				if smp.quantile != "" {
					lb.Add("quantile", smp.quantile)
				}
				lb.Sort()
				if _, err := app.Append(0, lb.Labels(), ts, smp.value); err != nil {
					failed++
					continue
				}
				appended++
			}
		}
	}
	if err := app.Commit(); err != nil {
		ing.logger.Warn("store ingest: commit failed", "err", err)
		ing.appendErrors.Add(float64(appended + failed))
		return
	}
	ing.samplesAppended.Add(float64(appended))
	if failed > 0 {
		ing.appendErrors.Add(float64(failed))
	}
}

// sample is one flattened time series value derived from a dto
// metric.
type sample struct {
	name     string
	value    float64
	le       string // histogram bucket bound, when non-empty
	quantile string // summary quantile, when non-empty
}

// flatten expands a dto.Metric into the same series a Prometheus
// scrape of the text exposition would produce: counters and gauges
// map 1:1, histograms expand to _bucket/_sum/_count, summaries to
// quantile series plus _sum/_count.
func flatten(fam *dto.MetricFamily, m *dto.Metric) []sample {
	name := fam.GetName()
	switch fam.GetType() {
	case dto.MetricType_COUNTER:
		return []sample{{name: name, value: m.GetCounter().GetValue()}}
	case dto.MetricType_GAUGE:
		return []sample{{name: name, value: m.GetGauge().GetValue()}}
	case dto.MetricType_HISTOGRAM:
		h := m.GetHistogram()
		out := make([]sample, 0, len(h.GetBucket())+3)
		infSeen := false
		for _, b := range h.GetBucket() {
			ub := b.GetUpperBound()
			out = append(out, sample{
				name:  name + "_bucket",
				value: float64(b.GetCumulativeCount()),
				le:    formatBound(ub),
			})
			if math.IsInf(ub, +1) {
				infSeen = true
			}
		}
		if !infSeen {
			out = append(out, sample{name: name + "_bucket", value: float64(h.GetSampleCount()), le: "+Inf"})
		}
		out = append(out,
			sample{name: name + "_sum", value: h.GetSampleSum()},
			sample{name: name + "_count", value: float64(h.GetSampleCount())},
		)
		return out
	case dto.MetricType_SUMMARY:
		s := m.GetSummary()
		out := make([]sample, 0, len(s.GetQuantile())+2)
		for _, q := range s.GetQuantile() {
			out = append(out, sample{name: name, value: q.GetValue(), quantile: formatBound(q.GetQuantile())})
		}
		out = append(out,
			sample{name: name + "_sum", value: s.GetSampleSum()},
			sample{name: name + "_count", value: float64(s.GetSampleCount())},
		)
		return out
	case dto.MetricType_UNTYPED:
		return []sample{{name: name, value: m.GetUntyped().GetValue()}}
	default:
		return nil
	}
}

// formatBound renders a bucket bound / quantile the way the text
// exposition format does, so series identity matches a real scrape.
func formatBound(v float64) string {
	if math.IsInf(v, +1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
