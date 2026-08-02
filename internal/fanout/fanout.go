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
	scheme  string
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
	// CAFile switches the agent dials to verified HTTPS (ADR-0029,
	// #197): the PEM CA bundle the agents' serving certs chain to,
	// typically ca.crt from the ollie-query-serving Secret. Empty
	// keeps plaintext HTTP (dev / tests) — there is deliberately no
	// skip-verify mode in between.
	CAFile string
	// ServerName is the DNS SAN to verify the agents' certs against.
	// Agents are dialed by pod IP, but they all serve one cert whose
	// SANs are the headless-Service DNS forms, so the client overrides
	// SNI/verification with that name (usually the same FQDN the
	// Discoverer resolves).
	ServerName string
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
	scheme := "http"
	if cfg.CAFile != "" {
		scheme = "https"
		auth.TLSConfig = promconfig.TLSConfig{
			CAFile:     cfg.CAFile,
			ServerName: cfg.ServerName,
		}
	}
	return &Queryable{
		disc:    cfg.Discoverer,
		auth:    auth,
		scheme:  scheme,
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
		clients: map[string]remote.ReadClient{},
	}
}

// client returns the cached read client for addr, building it on first
// use. Construction fails while the CA file is still missing (fresh
// install, before kubelet mounts the serving Secret); the error is not
// cached, so the next Select retries and the path self-heals once the
// file lands.
func (q *Queryable) client(addr string) (remote.ReadClient, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if c, ok := q.clients[addr]; ok {
		return c, nil
	}
	u, err := url.Parse(fmt.Sprintf("%s://%s/api/v1/read", q.scheme, addr))
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
			// Typically the CA file not yet mounted (ADR-0029 fresh
			// install). The agent must count as MISSING, not silently
			// absent — a stand-in querier records the miss at Select
			// time (Stats travel in the Select ctx), so the response
			// is flagged degraded like any other agent failure.
			q.logger.Warn("fanout: client build failed", "addr", addr, "err", err)
			queriers = append(queriers, &missQuerier{addr: addr})
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
	// Every agent is a SECONDARY querier, not a primary. This is the
	// load-bearing availability choice (storage-and-query.md §5.3):
	//
	//   - Primaries Select sequentially and a Select error aborts the
	//     whole query; secondaries Select CONCURRENTLY (the merge
	//     querier only parallelises when len(secondaries) > 0) and a
	//     first-iteration error is converted to a warning, not a
	//     failure. Serial fan-out over N agents also stacked N per-
	//     agent deadlines back-to-back; concurrent fan-out spends one.
	//
	// Prometheus's secondary handling only downgrades errors seen on
	// the FIRST Next(); a STREAMED_XOR_CHUNKS drain that fails mid-
	// stream (a deadline firing, an agent dying after headers) still
	// propagates through SeriesSet.Err() and would abort the query.
	// agentSeriesSet closes that gap by swallowing terminal Err() and
	// recording the miss, so a mid-drain agent failure degrades the
	// result instead of failing it.
	return storage.NewMergeQuerier(nil, queriers, storage.ChainedSeriesMerge), nil
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
	// completion instead. The set also swallows a terminal drain error
	// (deadline, mid-stream agent death) into a recorded miss.
	return &agentSeriesSet{
		SeriesSet: ss,
		cancel:    cancel,
		addr:      aq.addr,
		stats:     statsFrom(ctx),
		logger:    aq.logger,
	}
}

func (aq *agentQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (aq *agentQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (aq *agentQuerier) Close() error { return nil }

// agentSeriesSet wraps one agent's remote-read result. It releases the
// per-agent deadline context once the set is exhausted, and — crucially
// for availability — converts a terminal drain error into a recorded
// miss so a mid-stream agent failure degrades the query rather than
// aborting it (Prometheus's secondary-querier error tolerance only
// covers the first Next(); STREAMED_XOR_CHUNKS failures surface later,
// via Err()).
type agentSeriesSet struct {
	storage.SeriesSet
	cancel context.CancelFunc
	addr   string
	stats  *Stats
	logger *slog.Logger
	done   bool
}

func (c *agentSeriesSet) Next() bool {
	ok := c.SeriesSet.Next()
	if !ok && !c.done {
		c.done = true
		c.cancel()
	}
	return ok
}

// Err swallows a terminal drain error: the partial series already
// yielded are kept, the agent is recorded as missed (degraded=true +
// missingNodes), and nil is returned so the merge querier does not
// abort the whole fan-out on one agent's mid-stream failure.
func (c *agentSeriesSet) Err() error {
	if err := c.SeriesSet.Err(); err != nil {
		if !c.done {
			c.done = true
			c.cancel()
		}
		if c.logger != nil {
			c.logger.Warn("fanout: agent stream failed mid-drain; continuing degraded", "addr", c.addr, "err", err)
		}
		if c.stats != nil {
			c.stats.miss(c.addr)
		}
		return nil
	}
	return nil
}

// missQuerier stands in for an agent whose read client could not be
// built (e.g. --agent-ca-file not yet mounted, ADR-0029). It records
// the miss on the query's Stats at Select time so the answer degrades
// exactly like an agent that failed mid-read, and never errors the
// merge.
type missQuerier struct {
	addr string
}

func (m *missQuerier) Select(ctx context.Context, _ bool, _ *storage.SelectHints, _ ...*labels.Matcher) storage.SeriesSet {
	if s := statsFrom(ctx); s != nil {
		s.miss(m.addr)
	}
	return storage.EmptySeriesSet()
}

func (m *missQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (m *missQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (m *missQuerier) Close() error { return nil }
