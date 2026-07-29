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

// Package store embeds the Prometheus tsdb as the agent's node-local
// metric store (ADR-0002, ADR-0012, ADR-0025). The store holds a short
// retention window (default 10 minutes) of every sample the agent
// exposes on its scrape endpoint; the query server fans PromQL reads
// out across the per-node stores.
//
// Per ADR-0025 the full tsdb.DB is embedded via tsdb.Open rather than
// a hand-assembled HEAD: the DB owns WAL replay, head→block
// compaction, and retention deletion, which is exactly the lifecycle
// we would otherwise re-implement.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
)

// Config carries the store knobs. The zero value is completed by
// defaults matching ADR-0012.
type Config struct {
	// Dir is the tsdb data directory (blocks + WAL). Created if
	// absent.
	Dir string

	// BlockDuration is both the minimum and maximum tsdb block
	// duration; ADR-0012 pins 2 minutes so blocks match the snapshot
	// cadence. Defaults to 2m.
	BlockDuration time.Duration

	// Retention bounds how far back samples are kept. Defaults to
	// 10m (5 blocks).
	Retention time.Duration

	// Logger receives tsdb's own logging. Defaults to slog.Default.
	Logger *slog.Logger

	// MetricsRegisterer, when set, receives the store's bounded
	// self-observability set (ollie_store_compactions_total,
	// ollie_store_wal_fsync_seconds) bridged from tsdb's internal
	// registry — the other ~100 tsdb series stay off the agent's
	// scrape surface (ADR-0026 §4).
	MetricsRegisterer prometheus.Registerer
}

func (c *Config) applyDefaults() {
	if c.BlockDuration <= 0 {
		c.BlockDuration = 2 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 10 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Store is the node-local metric store. It is safe for concurrent
// use; the underlying tsdb serializes appends per appender and
// queriers see committed data.
type Store struct {
	db *tsdb.DB
}

// New opens (or re-opens, replaying the WAL) the store at cfg.Dir.
func New(cfg Config) (*Store, error) {
	cfg.applyDefaults()
	if cfg.Dir == "" {
		return nil, fmt.Errorf("store: Config.Dir is required")
	}

	opts := tsdb.DefaultOptions()
	opts.MinBlockDuration = cfg.BlockDuration.Milliseconds()
	opts.MaxBlockDuration = cfg.BlockDuration.Milliseconds()
	opts.RetentionDuration = cfg.Retention.Milliseconds()
	// The data dir lives on an emptyDir owned by exactly one agent
	// pod; a stale lockfile after a container crash must not wedge
	// the restart.
	opts.NoLockfile = true

	// tsdb registers its internals on a private registry; a bridge
	// re-exports only the allowlisted pair under ollie_store_* names
	// so the agent's bounded :9090 surface doesn't grow ~100 series.
	tsdbReg := prometheus.NewRegistry()
	db, err := tsdb.Open(cfg.Dir, cfg.Logger.With("component", "tsdb"), tsdbReg, opts, nil)
	if err != nil {
		return nil, fmt.Errorf("store: open tsdb at %s: %w", cfg.Dir, err)
	}
	if cfg.MetricsRegisterer != nil {
		cfg.MetricsRegisterer.MustRegister(&bridgeCollector{
			src: tsdbReg,
			rename: map[string]string{
				"prometheus_tsdb_compactions_total": "ollie_store_compactions_total",
				// tsdb exposes fsync latency as a summary; the §8
				// histogram shape is not available without forking
				// tsdb's metric registration.
				"prometheus_tsdb_wal_fsync_duration_seconds": "ollie_store_wal_fsync_seconds",
			},
		})
	}
	return &Store{db: db}, nil
}

// Compact triggers a head→block compaction cycle (which also
// enforces retention). tsdb runs this on its own schedule; the
// explicit trigger exists for tests and future admin tooling.
func (s *Store) Compact(ctx context.Context) error {
	return s.db.Compact(ctx)
}

// Appender returns a batched appender; callers must Commit or
// Rollback.
func (s *Store) Appender(ctx context.Context) storage.Appender {
	return s.db.Appender(ctx)
}

// Querier returns a querier over [mint, maxt] in Unix milliseconds.
func (s *Store) Querier(mint, maxt int64) (storage.Querier, error) {
	return s.db.Querier(mint, maxt)
}

// Queryable exposes the store to the PromQL engine.
func (s *Store) Queryable() storage.Queryable {
	return s.db
}

// ReadQueryable exposes the store to the Prometheus remote-read
// handler, which wants chunk access for the streamed response type.
func (s *Store) ReadQueryable() storage.SampleAndChunkQueryable {
	return s.db
}

// NumActiveSeries reports the number of series currently in the head
// block.
func (s *Store) NumActiveSeries() uint64 {
	return s.db.Head().NumSeries()
}

// Close flushes and closes the tsdb. The store is unusable afterward.
func (s *Store) Close() error {
	return s.db.Close()
}
