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
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// bridgeCollector re-exports an allowlisted, renamed subset of a
// source registry (tsdb's internals) onto the agent's public
// registry, keeping the rest off the bounded scrape surface
// (ADR-0026 §4).
type bridgeCollector struct {
	src    prometheus.Gatherer
	rename map[string]string // source family name → exported name
}

// Describe is intentionally empty: like the OBI forwarder, this is
// an unchecked collector whose series set follows the source.
func (b *bridgeCollector) Describe(chan<- *prometheus.Desc) {}

func (b *bridgeCollector) Collect(ch chan<- prometheus.Metric) {
	fams, err := b.src.Gather()
	if err != nil && fams == nil {
		return
	}
	for _, fam := range fams {
		newName, ok := b.rename[fam.GetName()]
		if !ok {
			continue
		}
		for _, m := range fam.GetMetric() {
			var labelKeys, labelVals []string
			for _, lp := range m.GetLabel() {
				labelKeys = append(labelKeys, lp.GetName())
				labelVals = append(labelVals, lp.GetValue())
			}
			desc := prometheus.NewDesc(newName, fam.GetHelp(), labelKeys, nil)
			var out prometheus.Metric
			var err error
			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				out, err = prometheus.NewConstMetric(desc, prometheus.CounterValue, m.GetCounter().GetValue(), labelVals...)
			case dto.MetricType_GAUGE:
				out, err = prometheus.NewConstMetric(desc, prometheus.GaugeValue, m.GetGauge().GetValue(), labelVals...)
			case dto.MetricType_SUMMARY:
				s := m.GetSummary()
				q := make(map[float64]float64, len(s.GetQuantile()))
				for _, sq := range s.GetQuantile() {
					q[sq.GetQuantile()] = sq.GetValue()
				}
				out, err = prometheus.NewConstSummary(desc, s.GetSampleCount(), s.GetSampleSum(), q, labelVals...)
			case dto.MetricType_HISTOGRAM:
				h := m.GetHistogram()
				buckets := make(map[float64]uint64, len(h.GetBucket()))
				for _, hb := range h.GetBucket() {
					buckets[hb.GetUpperBound()] = hb.GetCumulativeCount()
				}
				out, err = prometheus.NewConstHistogram(desc, h.GetSampleCount(), h.GetSampleSum(), buckets, labelVals...)
			default:
				continue
			}
			if err == nil {
				ch <- out
			}
		}
	}
}
