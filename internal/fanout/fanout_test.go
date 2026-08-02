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

package fanout

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	promcfg "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/util/annotations"

	"github.com/gke-labs/in-cluster-observability/internal/ca"
	"github.com/gke-labs/in-cluster-observability/internal/store"
)

// fakeAgent is a node-local store fronted by the same remote-read
// handler the real agent serves.
type fakeAgent struct {
	store *store.Store
	srv   *httptest.Server
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	st, err := store.New(store.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	rh := remote.NewReadHandler(slog.Default(), nil, st.ReadQueryable(),
		func() promcfg.Config { return promcfg.Config{} }, 5e7, 10, 1<<20)
	srv := httptest.NewServer(rh)
	t.Cleanup(srv.Close)
	return &fakeAgent{store: st, srv: srv}
}

func (a *fakeAgent) addr() string { return a.srv.Listener.Addr().String() }

func (a *fakeAgent) write(t *testing.T, node string, ts time.Time, v float64) {
	t.Helper()
	app := a.store.Appender(t.Context())
	lbls := labels.FromStrings(labels.MetricName, "test_requests_total", "k8s_node_name", node)
	if _, err := app.Append(0, lbls, ts.UnixMilli(), v); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func setup(t *testing.T, addrs []string) (*Queryable, *promql.Engine) {
	t.Helper()
	disc := NewDiscoverer("unused.invalid", 0, time.Hour, nil)
	disc.SetStaticAddrs(addrs)
	q := NewQueryable(Config{Discoverer: disc, Timeout: 10 * time.Second})
	eng := promql.NewEngine(promql.EngineOpts{
		MaxSamples:               1_000_000,
		Timeout:                  10 * time.Second,
		NoStepSubqueryIntervalFn: func(int64) int64 { return time.Minute.Milliseconds() },
	})
	return q, eng
}

// TestFanoutSum is #95's core acceptance shape: per-node values on
// separate agents aggregate to the cluster total.
func TestFanoutSum(t *testing.T) {
	now := time.Now()
	a1, a2, a3 := newFakeAgent(t), newFakeAgent(t), newFakeAgent(t)
	a1.write(t, "node-1", now, 10)
	a2.write(t, "node-2", now, 20)
	a3.write(t, "node-3", now, 30)

	q, eng := setup(t, []string{a1.addr(), a2.addr(), a3.addr()})

	ctx, stats := WithStats(t.Context())
	qry, err := eng.NewInstantQuery(ctx, q, nil, `sum(test_requests_total)`, now)
	if err != nil {
		t.Fatalf("NewInstantQuery: %v", err)
	}
	defer qry.Close()
	res := qry.Exec(ctx)
	if res.Err != nil {
		t.Fatalf("Exec: %v", res.Err)
	}
	vec, err := res.Vector()
	if err != nil {
		t.Fatalf("Vector: %v", err)
	}
	if len(vec) != 1 || vec[0].F != 60 {
		t.Fatalf("sum = %v, want 60", vec)
	}
	if stats.Degraded() {
		t.Fatalf("unexpected degraded: missing=%v", stats.Missing())
	}

	// Per-node series survive the merge with their identity.
	qry2, err := eng.NewInstantQuery(ctx, q, nil, `test_requests_total`, now)
	if err != nil {
		t.Fatalf("NewInstantQuery: %v", err)
	}
	defer qry2.Close()
	res2 := qry2.Exec(ctx)
	if res2.Err != nil {
		t.Fatalf("Exec: %v", res2.Err)
	}
	vec2, _ := res2.Vector()
	if len(vec2) != 3 {
		t.Fatalf("raw series = %d, want 3 (%v)", len(vec2), vec2)
	}
}

// TestFanoutDegraded is #95's second acceptance: kill one agent, the
// query returns the survivors' data flagged degraded.
func TestFanoutDegraded(t *testing.T) {
	now := time.Now()
	a1, a2, dead := newFakeAgent(t), newFakeAgent(t), newFakeAgent(t)
	a1.write(t, "node-1", now, 10)
	a2.write(t, "node-2", now, 20)
	deadAddr := dead.addr()
	dead.srv.Close()

	q, eng := setup(t, []string{a1.addr(), a2.addr(), deadAddr})

	ctx, stats := WithStats(t.Context())
	qry, err := eng.NewInstantQuery(ctx, q, nil, `sum(test_requests_total)`, now)
	if err != nil {
		t.Fatalf("NewInstantQuery: %v", err)
	}
	defer qry.Close()
	res := qry.Exec(ctx)
	if res.Err != nil {
		t.Fatalf("Exec should succeed degraded, got: %v", res.Err)
	}
	vec, _ := res.Vector()
	if len(vec) != 1 || vec[0].F != 30 {
		t.Fatalf("degraded sum = %v, want 30 from survivors", vec)
	}
	if !stats.Degraded() {
		t.Fatal("expected degraded=true")
	}
	missing := stats.Missing()
	if len(missing) != 1 || missing[0] != deadAddr {
		t.Fatalf("missing = %v, want [%s]", missing, deadAddr)
	}
}

// fakeSeriesSet yields n series, then reports err from Err(). Models a
// STREAMED_XOR_CHUNKS remote read that opens cleanly (Read returns at
// headers) but fails mid-drain — the failure mode Prometheus's
// secondary-querier error tolerance does NOT cover, since it only
// downgrades errors seen on the first Next().
type fakeSeriesSet struct {
	n   int
	err error
}

func (s *fakeSeriesSet) Next() bool {
	if s.n > 0 {
		s.n--
		return true
	}
	return false
}
func (s *fakeSeriesSet) At() storage.Series                { return nil }
func (s *fakeSeriesSet) Err() error                        { return s.err }
func (s *fakeSeriesSet) Warnings() annotations.Annotations { return nil }

// TestAgentSeriesSetSwallowsMidStreamError verifies a mid-drain agent
// failure degrades the fan-out (recorded miss, nil error) rather than
// aborting the whole query, and releases the per-agent deadline context.
func TestAgentSeriesSetSwallowsMidStreamError(t *testing.T) {
	stats := &Stats{}
	canceled := false
	ss := &agentSeriesSet{
		SeriesSet: &fakeSeriesSet{n: 2, err: errors.New("stream reset mid-chunk")},
		cancel:    func() { canceled = true },
		addr:      "10.0.0.9:9091",
		stats:     stats,
		logger:    slog.Default(),
	}
	for ss.Next() {
	}
	if err := ss.Err(); err != nil {
		t.Fatalf("terminal error must be swallowed, got %v", err)
	}
	if !stats.Degraded() {
		t.Fatal("expected the failed agent to be recorded as a miss")
	}
	if m := stats.Missing(); len(m) != 1 || m[0] != "10.0.0.9:9091" {
		t.Fatalf("missing = %v, want [10.0.0.9:9091]", m)
	}
	if !canceled {
		t.Fatal("per-agent deadline context was not released")
	}
}

// TestAgentSeriesSetCleanDrain confirms a healthy stream records no
// miss and reports no error.
func TestAgentSeriesSetCleanDrain(t *testing.T) {
	stats := &Stats{}
	ss := &agentSeriesSet{
		SeriesSet: &fakeSeriesSet{n: 3},
		cancel:    func() {},
		addr:      "10.0.0.1:9091",
		stats:     stats,
		logger:    slog.Default(),
	}
	for ss.Next() {
	}
	if err := ss.Err(); err != nil {
		t.Fatalf("clean drain must not error, got %v", err)
	}
	if stats.Degraded() {
		t.Fatalf("clean drain must not record a miss: %v", stats.Missing())
	}
}

// TestDiscovererStatic covers Ready gating.
func TestDiscovererStatic(t *testing.T) {
	d := NewDiscoverer("unused.invalid", 9091, time.Hour, nil)
	if d.Ready() {
		t.Fatal("Ready() true with no endpoints")
	}
	d.SetStaticAddrs([]string{"10.0.0.1:9091"})
	if !d.Ready() {
		t.Fatal("Ready() false after SetStaticAddrs")
	}
	if got := d.Addrs(); len(got) != 1 || got[0] != "10.0.0.1:9091" {
		t.Fatalf("Addrs() = %v", got)
	}
}

// Phase 2b (#197, ADR-0029): with a CAFile the fan-out dials verified
// HTTPS — succeeding against a CA-issued serving cert, treating a
// server outside the CA (e.g. a bootstrap self-signed cert) as a miss,
// and self-healing when the CA file appears after startup.
func TestFanoutTLS(t *testing.T) {
	now := time.Now()

	authority, err := ca.NewCA(now, ca.CADefaultLifetime)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := authority.IssueServingCert([]string{"ollie-agent.ollie-system.svc"}, now, ca.ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("IssueServingCert: %v", err)
	}
	keypair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	a := newFakeAgent(t)
	a.write(t, "node-1", now, 10)
	// Re-serve the same handler over TLS with the CA-issued cert.
	tlsSrv := httptest.NewUnstartedServer(a.srv.Config.Handler)
	tlsSrv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}}
	tlsSrv.StartTLS()
	t.Cleanup(tlsSrv.Close)
	addr := tlsSrv.Listener.Addr().String()

	caFile := filepath.Join(t.TempDir(), "ca.crt")

	disc := NewDiscoverer("unused.invalid", 0, time.Hour, nil)
	disc.SetStaticAddrs([]string{addr})
	q := NewQueryable(Config{
		Discoverer: disc,
		CAFile:     caFile,
		ServerName: "ollie-agent.ollie-system.svc",
		Timeout:    10 * time.Second,
	})
	eng := promql.NewEngine(promql.EngineOpts{
		MaxSamples:               1_000_000,
		Timeout:                  10 * time.Second,
		NoStepSubqueryIntervalFn: func(int64) int64 { return time.Minute.Milliseconds() },
	})

	run := func() (float64, *Stats, error) {
		ctx, stats := WithStats(t.Context())
		qry, err := eng.NewInstantQuery(ctx, q, nil, `sum(test_requests_total)`, now)
		if err != nil {
			t.Fatalf("NewInstantQuery: %v", err)
		}
		defer qry.Close()
		res := qry.Exec(ctx)
		if res.Err != nil {
			return 0, stats, res.Err
		}
		vec, err := res.Vector()
		if err != nil || len(vec) == 0 {
			return 0, stats, err
		}
		return vec[0].F, stats, nil
	}

	// CA file missing: the dial must fail closed (degraded), and the
	// failure must not be cached.
	if _, stats, _ := run(); !stats.Degraded() {
		t.Fatal("fan-out succeeded without a CA file; want fail-closed miss")
	}

	// CA file lands (kubelet mounts the Secret): the same Queryable
	// self-heals and the verified read succeeds.
	if err := os.WriteFile(caFile, authority.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	v, stats, err := run()
	if err != nil {
		t.Fatalf("verified read: %v", err)
	}
	if stats.Degraded() || v != 10 {
		t.Fatalf("verified read = %v (degraded=%v, missing=%v), want 10 healthy", v, stats.Degraded(), stats.Missing())
	}

	// A server the CA never signed is a miss, not data.
	foreign := newFakeAgent(t)
	foreignTLS := httptest.NewTLSServer(foreign.srv.Config.Handler)
	t.Cleanup(foreignTLS.Close)
	disc.SetStaticAddrs([]string{foreignTLS.Listener.Addr().String()})
	if _, stats, _ := run(); !stats.Degraded() {
		t.Fatal("fan-out accepted a cert outside the ollie CA")
	}
}
