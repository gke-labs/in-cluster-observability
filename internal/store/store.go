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

	// The registerer is nil deliberately: tsdb's ~100 internal series
	// would otherwise land on the agent's bounded :9090 surface. The
	// ingester exposes the small ollie_store_* set instead.
	db, err := tsdb.Open(cfg.Dir, cfg.Logger.With("component", "tsdb"), nil, opts, nil)
	if err != nil {
		return nil, fmt.Errorf("store: open tsdb at %s: %w", cfg.Dir, err)
	}
	return &Store{db: db}, nil
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

// NumActiveSeries reports the number of series currently in the head
// block.
func (s *Store) NumActiveSeries() uint64 {
	return s.db.Head().NumSeries()
}

// Close flushes and closes the tsdb. The store is unusable afterward.
func (s *Store) Close() error {
	return s.db.Close()
}
