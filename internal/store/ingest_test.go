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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestIngesterTick verifies one gather-and-append cycle lands the
// same series a text-format scrape would produce, including the
// histogram expansion.
func TestIngesterTick(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	reg := prometheus.NewRegistry()

	ctr := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_ingest_requests_total",
		Help: "test",
	}, []string{"code"})
	ctr.WithLabelValues("200").Add(3)
	ctr.WithLabelValues("500").Add(1)

	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_ingest_up",
		Help: "test",
	})
	gauge.Set(1)

	hist := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "test_ingest_duration_seconds",
		Help:    "test",
		Buckets: []float64{0.1, 1},
	})
	hist.Observe(0.05)
	hist.Observe(0.5)
	hist.Observe(5)

	reg.MustRegister(ctr, gauge, hist)

	ing := NewIngester(s, reg, reg, time.Second, nil)
	ing.Tick(context.Background())
	now := time.Now()

	for q, want := range map[string]float64{
		`test_ingest_requests_total{code="200"}`:                       3,
		`test_ingest_requests_total{code="500"}`:                       1,
		`test_ingest_up`:                                               1,
		`test_ingest_duration_seconds_bucket{le="0.1"}`:                1,
		`test_ingest_duration_seconds_bucket{le="1"}`:                  2,
		`test_ingest_duration_seconds_bucket{le="+Inf"}`:               3,
		`test_ingest_duration_seconds_count`:                           3,
		`histogram_quantile(0.5, test_ingest_duration_seconds_bucket)`: 0.55,
	} {
		vec := queryInstant(t, s, q, now)
		if len(vec) != 1 {
			t.Errorf("%s: got %d samples, want 1", q, len(vec))
			continue
		}
		if got := vec[0].F; got != want {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}

	// Self-obs metrics are registered on the same registry, so the
	// next tick stores them too.
	ing.Tick(context.Background())
	vec := queryInstant(t, s, `ollie_store_samples_appended_total`, time.Now())
	if len(vec) != 1 || vec[0].F <= 0 {
		t.Errorf("ollie_store_samples_appended_total = %v, want > 0", vec)
	}
	vec = queryInstant(t, s, `ollie_store_active_series`, time.Now())
	if len(vec) != 1 || vec[0].F <= 0 {
		t.Errorf("ollie_store_active_series = %v, want > 0", vec)
	}
}
