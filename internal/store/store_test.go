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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
)

func newTestEngine(t *testing.T) *promql.Engine {
	t.Helper()
	return promql.NewEngine(promql.EngineOpts{
		MaxSamples: 1_000_000,
		Timeout:    30 * time.Second,
	})
}

func queryInstant(t *testing.T, s *Store, q string, ts time.Time) promql.Vector {
	t.Helper()
	eng := newTestEngine(t)
	qry, err := eng.NewInstantQuery(context.Background(), s.Queryable(), nil, q, ts)
	if err != nil {
		t.Fatalf("NewInstantQuery(%q): %v", q, err)
	}
	defer qry.Close()
	res := qry.Exec(context.Background())
	if res.Err != nil {
		t.Fatalf("Exec(%q): %v", q, res.Err)
	}
	vec, err := res.Vector()
	if err != nil {
		t.Fatalf("Vector(%q): %v", q, err)
	}
	return vec
}

func TestOpenAppendQueryClose(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	base := time.Now().Add(-time.Minute)
	app := s.Appender(context.Background())
	lbls := labels.FromStrings(labels.MetricName, "test_gauge", "k8s_pod_name", "p1")
	if _, err := app.Append(0, lbls, base.UnixMilli(), 42); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	vec := queryInstant(t, s, `test_gauge{k8s_pod_name="p1"}`, base)
	if len(vec) != 1 || vec[0].F != 42 {
		t.Fatalf("query returned %v, want single sample 42", vec)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestPromQLRoundTrip is #78's acceptance: 1000 samples across 50
// series written and read back through PromQL.
func TestPromQLRoundTrip(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	const (
		series  = 50
		samples = 20 // per series -> 1000 total
	)
	base := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	step := time.Second

	app := s.Appender(context.Background())
	for i := 0; i < samples; i++ {
		ts := base.Add(time.Duration(i) * step).UnixMilli()
		for j := 0; j < series; j++ {
			lbls := labels.FromStrings(
				labels.MetricName, "test_requests_total",
				"k8s_pod_name", fmt.Sprintf("pod-%02d", j),
			)
			// Monotonic per-series counter: i+1.
			if _, err := app.Append(0, lbls, ts, float64(i+1)); err != nil {
				t.Fatalf("Append(series %d, sample %d): %v", j, i, err)
			}
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	last := base.Add(time.Duration(samples-1) * step)

	vec := queryInstant(t, s, `count(test_requests_total)`, last)
	if len(vec) != 1 || vec[0].F != series {
		t.Fatalf("count() = %v, want %d", vec, series)
	}
	vec = queryInstant(t, s, `sum(test_requests_total)`, last)
	if want := float64(series * samples); len(vec) != 1 || vec[0].F != want {
		t.Fatalf("sum() = %v, want %v", vec, want)
	}
	// rate() over the window: each series grows 1/s.
	vec = queryInstant(t, s, `sum(rate(test_requests_total[15s]))`, last)
	if len(vec) != 1 || vec[0].F < series*0.9 || vec[0].F > series*1.1 {
		t.Fatalf("sum(rate()) = %v, want ~%d", vec, series)
	}
}

// TestWALRestart verifies a re-open replays the WAL and restores the
// head (#78 acceptance).
func TestWALRestart(t *testing.T) {
	dir := t.TempDir()

	s, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := time.Now().Add(-time.Minute)
	app := s.Appender(context.Background())
	lbls := labels.FromStrings(labels.MetricName, "test_persisted", "k8s_pod_name", "p1")
	if _, err := app.Append(0, lbls, base.UnixMilli(), 7); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// WAL files exist on disk before close.
	walDir := filepath.Join(dir, "wal")
	entries, err := os.ReadDir(walDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected WAL segments in %s, got entries=%v err=%v", walDir, entries, err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer s2.Close()

	vec := queryInstant(t, s2, `test_persisted`, base)
	if len(vec) != 1 || vec[0].F != 7 {
		t.Fatalf("after restart query = %v, want single sample 7", vec)
	}
}
