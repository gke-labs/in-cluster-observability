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

// Package fanout implements the query server's cross-agent read path
// (ADR-0025 §3): a Prometheus storage.Queryable whose Select fans out
// to every discovered agent's remote-read endpoint and merges the raw
// series, so the stock PromQL engine evaluates centrally with exact
// semantics.
//
// Availability beats completeness (storage-and-query.md §5.3): an
// agent that misses its per-query deadline is skipped, the query
// proceeds on the remaining agents, and the miss is recorded on the
// query's Stats for the API layer to surface as degraded=true.
package fanout

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"sync"
	"time"

	promconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/util/annotations"
)

// deadlineFraction is the share of the overall query deadline granted
// to each per-agent read (storage-and-query.md §5.1).
const deadlineFraction = 0.8

// Discoverer resolves the agent headless Service to a set of
// endpoint addresses on an interval.
type Discoverer struct {
	service  string
	port     int
	interval time.Duration
	logger   *slog.Logger
	resolver *net.Resolver

	mu    sync.RWMutex
	addrs []string
}

// NewDiscoverer resolves service (a headless-Service DNS name) to
// host:port endpoints every interval (default 15s).
func NewDiscoverer(service string, port int, interval time.Duration, logger *slog.Logger) *Discoverer {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Discoverer{
		service:  service,
		port:     port,
		interval: interval,
		logger:   logger,
		resolver: net.DefaultResolver,
	}
}

// Run resolves once immediately, then on every tick until ctx ends.
func (d *Discoverer) Run(ctx context.Context) {
	d.resolve(ctx)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.resolve(ctx)
		}
	}
}

func (d *Discoverer) resolve(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := d.resolver.LookupIP(rctx, "ip4", d.service)
	if err != nil {
		// Keep the last-known set: a DNS blip must not blank the
		// fan-out target list.
		d.logger.Warn("agent discovery: lookup failed; keeping previous endpoints", "service", d.service, "err", err)
		return
	}
	addrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.JoinHostPort(ip.String(), fmt.Sprintf("%d", d.port)))
	}
	sort.Strings(addrs)
	d.mu.Lock()
	changed := len(addrs) != len(d.addrs)
	if !changed {
		for i := range addrs {
			if addrs[i] != d.addrs[i] {
				changed = true
				break
			}
		}
	}
	d.addrs = addrs
	d.mu.Unlock()
	if changed {
		d.logger.Info("agent discovery: endpoints updated", "count", len(addrs), "addrs", addrs)
	}
}

// Addrs returns the current endpoint set.
func (d *Discoverer) Addrs() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]string(nil), d.addrs...)
}

// Ready reports whether at least one agent endpoint is known
// (backs /healthz/ready, #94).
func (d *Discoverer) Ready() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.addrs) > 0
}

// SetStaticAddrs replaces discovery with a fixed endpoint set (tests
// and --agent-addrs debugging).
func (d *Discoverer) SetStaticAddrs(addrs []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append([]string(nil), addrs...)
}

// Stats accumulates per-query fan-out health; the API layer surfaces
// it as degraded=true + missing_nodes.
type Stats struct {
	mu      sync.Mutex
	missing []string
}

// Missing lists the agent endpoints that failed or timed out.
func (s *Stats) Missing() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.missing...)
}

// Degraded reports whether any agent was skipped.
func (s *Stats) Degraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.missing) > 0
}

func (s *Stats) miss(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.missing {
		if m == addr {
			return
		}
	}
	s.missing = append(s.missing, addr)
}

type statsKey struct{}

// WithStats derives a context carrying a fresh Stats. The PromQL
// engine propagates the query context into Select, so per-agent
// misses recorded during evaluation surface on the same Stats the
// API handler holds.
func WithStats(ctx context.Context) (context.Context, *Stats) {
	s := &Stats{}
	return context.WithValue(ctx, statsKey{}, s), s
}

func statsFrom(ctx context.Context) *Stats {
	s, _ := ctx.Value(statsKey{}).(*Stats)
	return s
}

// Queryable fans Select out across the discovered agents.
type Queryable struct {
	disc    *Discoverer
	auth    promconfig.HTTPClientConfig
	timeout time.Duration
	logger  *slog.Logger

	mu      sync.Mutex
	clients map[string]remote.ReadClient
}

// Config for NewQueryable.
type Config struct {
	Discoverer *Discoverer
	// BearerTokenFile, when set, authenticates reads to the agents
	// (the agent guards /api/v1/read with TokenReview + SAR).
	BearerTokenFile string
	// Timeout caps a single remote read call (transport-level;
	// per-query deadlines are usually tighter).
	Timeout time.Duration
	Logger  *slog.Logger
}

// NewQueryable builds the fan-out queryable.
func NewQueryable(cfg Config) *Queryable {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	auth := promconfig.HTTPClientConfig{}
	if cfg.BearerTokenFile != "" {
		auth.Authorization = &promconfig.Authorization{
			Type:            "Bearer",
			CredentialsFile: cfg.BearerTokenFile,
		}
	}
	return &Queryable{
		disc:    cfg.Discoverer,
		auth:    auth,
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
		clients: map[string]remote.ReadClient{},
	}
}

func (q *Queryable) client(addr string) (remote.ReadClient, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if c, ok := q.clients[addr]; ok {
		return c, nil
	}
	u, err := url.Parse(fmt.Sprintf("http://%s/api/v1/read", addr))
	if err != nil {
		return nil, err
	}
	c, err := remote.NewReadClient(addr, &remote.ClientConfig{
		URL:              &promconfig.URL{URL: u},
		Timeout:          model.Duration(q.timeout),
		HTTPClientConfig: q.auth,
		ChunkedReadLimit: config.DefaultChunkedReadLimit,
	})
	if err != nil {
		return nil, err
	}
	q.clients[addr] = c
	return c, nil
}

// Querier implements storage.Queryable by merging one querier per
// currently-discovered agent.
func (q *Queryable) Querier(mint, maxt int64) (storage.Querier, error) {
	addrs := q.disc.Addrs()
	queriers := make([]storage.Querier, 0, len(addrs))
	for _, addr := range addrs {
		c, err := q.client(addr)
		if err != nil {
			q.logger.Warn("fanout: client build failed", "addr", addr, "err", err)
			continue
		}
		queriers = append(queriers, &agentQuerier{
			client: c,
			addr:   addr,
			mint:   mint,
			maxt:   maxt,
			logger: q.logger,
		})
	}
	return storage.NewMergeQuerier(queriers, nil, storage.ChainedSeriesMerge), nil
}

// agentQuerier reads one agent's series over remote read.
type agentQuerier struct {
	client remote.ReadClient
	addr   string
	mint   int64
	maxt   int64
	logger *slog.Logger
}

func (aq *agentQuerier) Select(ctx context.Context, sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	query, err := remote.ToQuery(aq.mint, aq.maxt, matchers, hints)
	if err != nil {
		return storage.ErrSeriesSet(err)
	}

	// Per-agent deadline = 0.8× the remaining overall deadline, so a
	// hung agent leaves headroom to aggregate the others.
	cancel := func() {}
	if dl, ok := ctx.Deadline(); ok {
		budget := time.Duration(deadlineFraction * float64(time.Until(dl)))
		ctx, cancel = context.WithTimeout(ctx, budget)
	}

	ss, err := aq.client.Read(ctx, query, sortSeries)
	if err != nil {
		cancel()
		aq.logger.Warn("fanout: agent read failed; continuing degraded", "addr", aq.addr, "err", err)
		if s := statsFrom(ctx); s != nil {
			s.miss(aq.addr)
		}
		return storage.EmptySeriesSet()
	}
	// The merged set is drained within the query's lifetime; the
	// engine cancels the parent ctx when evaluation ends, so leaking
	// cancel to that point is acceptable — but tie it to iteration
	// completion instead.
	return &cancelingSeriesSet{SeriesSet: ss, cancel: cancel}
}

func (aq *agentQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (aq *agentQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (aq *agentQuerier) Close() error { return nil }

// cancelingSeriesSet releases the per-agent deadline context once the
// set is exhausted.
type cancelingSeriesSet struct {
	storage.SeriesSet
	cancel context.CancelFunc
	done   bool
}

func (c *cancelingSeriesSet) Next() bool {
	ok := c.SeriesSet.Next()
	if !ok && !c.done {
		c.done = true
		c.cancel()
	}
	return ok
}
