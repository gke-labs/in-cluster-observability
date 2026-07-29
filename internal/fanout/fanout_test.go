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
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	promcfg "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage/remote"

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
