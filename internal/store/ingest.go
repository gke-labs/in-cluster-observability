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
	"github.com/prometheus/prometheus/model/value"
)

// Ingester feeds the store by periodically gathering a Prometheus
// registry and appending every sample (ADR-0025 §2: the same registry
// backs the :9090 scrape endpoint, so what PromQL sees is exactly
// what a scraper would see — one normalization path).
// nodeLabel identifies the node that observed a stored series. The
// query server fans out to every agent's store and merges the raw
// series; without a per-node label, identical self-obs series
// (ollie_agent_up, ollie_store_*) from different nodes carry the same
// identity and collapse into one during the merge — a counter reset
// storm under rate(), and a silently wrong active-series total. The
// store is queried directly (no scraper in between to relabel target
// identity), so it must self-identify its node (#170 review).
const nodeLabel = "k8s_node_name"

type Ingester struct {
	store    *Store
	gatherer prometheus.Gatherer
	interval time.Duration
	logger   *slog.Logger
	nodeName string

	// prevSeries is the identity set of the previous tick's appended
	// series, used to emit staleness markers (#191). Only Tick reads
	// or writes it, and Run calls Tick sequentially.
	prevSeries map[uint64]labels.Labels

	samplesAppended prometheus.Counter
	appendErrors    prometheus.Counter
	activeSeries    prometheus.GaugeFunc
}

// NewIngester wires an ingester gathering from g every interval
// (default 1s). Self-observability metrics are registered on reg,
// which is typically the same registry g gathers — the ollie_store_*
// series then flow into the store like everything else. nodeName, when
// non-empty, is stamped onto every appended series as k8s_node_name so
// the query server's cross-node merge keeps per-node series distinct.
func NewIngester(s *Store, g prometheus.Gatherer, reg prometheus.Registerer, interval time.Duration, logger *slog.Logger, nodeName string) *Ingester {
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
		nodeName: nodeName,
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
	cur := make(map[uint64]labels.Labels, len(ing.prevSeries))
	for _, fam := range fams {
		for _, m := range fam.GetMetric() {
			for _, smp := range Flatten(fam, m) {
				lb := labels.NewScratchBuilder(len(m.GetLabel()) + 3)
				lb.Add(labels.MetricName, smp.Name)
				hasNode := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == nodeLabel {
						hasNode = true
					}
					lb.Add(lp.GetName(), lp.GetValue())
				}
				// Stamp the node identity unless the series already
				// carries one (an OBI metric could, in principle);
				// adding a duplicate label key would make the series
				// invalid.
				if ing.nodeName != "" && !hasNode {
					lb.Add(nodeLabel, ing.nodeName)
				}
				if smp.Le != "" {
					lb.Add(labels.BucketLabel, smp.Le)
				}
				if smp.Quantile != "" {
					lb.Add("quantile", smp.Quantile)
				}
				lb.Sort()
				l := lb.Labels()
				if _, err := app.Append(0, l, ts, smp.Value); err != nil {
					failed++
					continue
				}
				cur[l.Hash()] = l
				appended++
			}
		}
	}
	// Staleness markers (#191): a series that was appended last tick
	// but is gone this tick (unregistered collector, evicted OBI
	// series after pod churn) would otherwise stay frozen at its last
	// value for the PromQL lookback window (~5m) — visible downstream
	// as a live series that is actually dead. Appending an explicit
	// StaleNaN makes it disappear from queries immediately, exactly as
	// a real Prometheus scrape would on target churn.
	for h, l := range ing.prevSeries {
		if _, ok := cur[h]; ok {
			continue
		}
		if _, err := app.Append(0, l, ts, math.Float64frombits(value.StaleNaN)); err != nil {
			failed++
		}
	}
	ing.prevSeries = cur
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

// Sample is one flattened time series value derived from a dto
// metric. Shared by the tsdb ingester and the remote-write exporter
// so both surfaces emit identical series (ADR-0025 §2 one
// normalization path).
type Sample struct {
	Name     string
	Value    float64
	Le       string // histogram bucket bound, when non-empty
	Quantile string // summary quantile, when non-empty
}

// Flatten expands a dto.Metric into the same series a Prometheus
// scrape of the text exposition would produce: counters and gauges
// map 1:1, histograms expand to _bucket/_sum/_count, summaries to
// quantile series plus _sum/_count.
func Flatten(fam *dto.MetricFamily, m *dto.Metric) []Sample {
	name := fam.GetName()
	switch fam.GetType() {
	case dto.MetricType_COUNTER:
		return []Sample{{Name: name, Value: m.GetCounter().GetValue()}}
	case dto.MetricType_GAUGE:
		return []Sample{{Name: name, Value: m.GetGauge().GetValue()}}
	case dto.MetricType_HISTOGRAM:
		h := m.GetHistogram()
		out := make([]Sample, 0, len(h.GetBucket())+3)
		infSeen := false
		for _, b := range h.GetBucket() {
			ub := b.GetUpperBound()
			out = append(out, Sample{
				Name:  name + "_bucket",
				Value: float64(b.GetCumulativeCount()),
				Le:    formatBound(ub),
			})
			if math.IsInf(ub, +1) {
				infSeen = true
			}
		}
		if !infSeen {
			out = append(out, Sample{Name: name + "_bucket", Value: float64(h.GetSampleCount()), Le: "+Inf"})
		}
		out = append(out,
			Sample{Name: name + "_sum", Value: h.GetSampleSum()},
			Sample{Name: name + "_count", Value: float64(h.GetSampleCount())},
		)
		return out
	case dto.MetricType_SUMMARY:
		s := m.GetSummary()
		out := make([]Sample, 0, len(s.GetQuantile())+2)
		for _, q := range s.GetQuantile() {
			out = append(out, Sample{Name: name, Value: q.GetValue(), Quantile: formatBound(q.GetQuantile())})
		}
		out = append(out,
			Sample{Name: name + "_sum", Value: s.GetSampleSum()},
			Sample{Name: name + "_count", Value: float64(s.GetSampleCount())},
		)
		return out
	case dto.MetricType_UNTYPED:
		return []Sample{{Name: name, Value: m.GetUntyped().GetValue()}}
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
