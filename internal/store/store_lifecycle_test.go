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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/labels"
)

// blockDirs lists ULID block directories under dir (excluding wal/
// chunks_head/ etc).
func blockDirs(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		// tsdb block ULIDs are 26-char uppercase Crockford base32.
		if e.IsDir() && len(e.Name()) == 26 && strings.ToUpper(e.Name()) == e.Name() {
			out[e.Name()] = true
		}
	}
	return out
}

// TestBlockRotationAndRetention is #79's acceptance, time-compressed:
// blocks form from the head and blocks past retention are dropped,
// and the bridged ollie_store_compactions_total ticks.
func TestBlockRotationAndRetention(t *testing.T) {
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	s, err := New(Config{
		Dir:               dir,
		BlockDuration:     2 * time.Second,
		Retention:         6 * time.Second,
		MetricsRegisterer: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Write a sample per 100ms across ~5 block durations, ending now.
	base := time.Now().Add(-10 * time.Second)
	app := s.Appender(t.Context())
	lbls := labels.FromStrings(labels.MetricName, "test_rotation")
	for ts := base; ts.Before(time.Now()); ts = ts.Add(100 * time.Millisecond) {
		if _, err := app.Append(0, lbls, ts.UnixMilli(), 1); err != nil {
			t.Fatalf("Append at %v: %v", ts, err)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := s.Compact(t.Context()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	initial := blockDirs(t, dir)
	if len(initial) == 0 {
		t.Fatal("no blocks after compaction; head never rotated")
	}

	// The bridge exposes the compaction counter under the ollie name.
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var sawCompactions, sawFsync bool
	for _, f := range fams {
		switch f.GetName() {
		case "ollie_store_compactions_total":
			sawCompactions = f.GetMetric()[0].GetCounter().GetValue() >= 1
		case "ollie_store_wal_fsync_seconds":
			sawFsync = true
		}
	}
	if !sawCompactions {
		t.Error("ollie_store_compactions_total missing or zero after Compact")
	}
	if !sawFsync {
		t.Error("ollie_store_wal_fsync_seconds not bridged")
	}

	// Age the original blocks past retention by appending newer data
	// (newest-block MaxTime is retention's reference point), then
	// compact until an original block is dropped. New blocks cut
	// from the head can keep the directory COUNT constant, so assert
	// on block identity, not count.
	app = s.Appender(t.Context())
	for off := 10 * time.Second; off <= 14*time.Second; off += 100 * time.Millisecond {
		if _, err := app.Append(0, lbls, time.Now().Add(off).UnixMilli(), 2); err != nil {
			t.Fatalf("Append future: %v", err)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	dropped := func() bool {
		current := blockDirs(t, dir)
		for name := range initial {
			if !current[name] {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(30 * time.Second)
	for !dropped() && time.Now().Before(deadline) {
		if err := s.Compact(t.Context()); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !dropped() {
		t.Fatalf("retention never dropped an original block: initial=%v current=%v", initial, blockDirs(t, dir))
	}
}
