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

package export

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func counterValue(t *testing.T, reg *prometheus.Registry, name string, labelPairs map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			for k, v := range labelPairs {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
					}
				}
				if !found {
					continue metric
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func fastRelay[T any](r *Relay[T]) *Relay[T] {
	r.initialBackoff = time.Millisecond
	r.maxBackoff = 2 * time.Millisecond
	return r
}

func TestRelayDeliversAndRetries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	var calls atomic.Int64
	r := fastRelay(NewRelay("ep", 4, m, nil, func(_ context.Context, _ int) error {
		if calls.Add(1) < 3 {
			return errors.New("transient")
		}
		return nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)

	r.Enqueue(1)
	deadline := time.Now().Add(5 * time.Second)
	for counterValue(t, reg, "ollie_export_batches_total", map[string]string{"endpoint": "ep"}) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("batch never delivered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("send calls = %d, want 3 (2 transient failures + success)", got)
	}
	if got := counterValue(t, reg, "ollie_export_errors_total", map[string]string{"endpoint": "ep", "kind": "transient"}); got != 2 {
		t.Fatalf("transient errors = %v, want 2", got)
	}
}

func TestRelayDropsOnExhaustionAndPermanent(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	rFail := fastRelay(NewRelay("dead", 4, m, nil, func(_ context.Context, _ int) error {
		return errors.New("always down")
	}))
	rPerm := fastRelay(NewRelay("perm", 4, m, nil, func(_ context.Context, _ int) error {
		return Permanent{Err: errors.New("bad request")}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go rFail.Run(ctx)
	go rPerm.Run(ctx)

	rFail.Enqueue(1)
	rPerm.Enqueue(1)

	deadline := time.Now().Add(5 * time.Second)
	for {
		exhausted := counterValue(t, reg, "ollie_export_dropped_total", map[string]string{"endpoint": "dead", "reason": "retry_exhausted"})
		permanent := counterValue(t, reg, "ollie_export_dropped_total", map[string]string{"endpoint": "perm", "reason": "permanent"})
		if exhausted == 1 && permanent == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drops not recorded: exhausted=%v permanent=%v", exhausted, permanent)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelayBufferFull(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	// No worker running: the queue fills.
	r := NewRelay("full", 2, m, nil, func(_ context.Context, _ int) error { return nil })
	for i := 0; i < 5; i++ {
		r.Enqueue(i)
	}
	if got := counterValue(t, reg, "ollie_export_dropped_total", map[string]string{"endpoint": "full", "reason": "buffer_full"}); got != 3 {
		t.Fatalf("buffer_full drops = %v, want 3", got)
	}
}

func metricsReq() *collmetricspb.ExportMetricsServiceRequest {
	return &collmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{Name: "test.metric"}},
			}},
		}},
	}
}

func TestOTLPHTTPRelay(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	type got struct {
		path, ctype, encoding, auth string
		body                        []byte
	}
	ch := make(chan got, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rd io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip reader: %v", err)
				w.WriteHeader(400)
				return
			}
			rd = zr
		}
		body, _ := io.ReadAll(rd)
		ch <- got{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), r.Header.Get("Authorization"), body}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	relays, err := NewOTLPRelays(OTLPConfig{
		Endpoint:    srv.URL,
		Protocol:    "http",
		Headers:     map[string]string{"Authorization": "Bearer tok"},
		Compression: "gzip",
		Metrics:     m,
	})
	if err != nil {
		t.Fatalf("NewOTLPRelays: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go relays.Run(ctx)

	want := metricsReq()
	relays.Metrics.Enqueue(want)

	select {
	case g := <-ch:
		if g.path != "/v1/metrics" || g.ctype != "application/x-protobuf" || g.encoding != "gzip" || g.auth != "Bearer tok" {
			t.Fatalf("request shape = %+v", g)
		}
		var decoded collmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(g.body, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetName() != "test.metric" {
			t.Fatalf("payload not relayed verbatim: %v", &decoded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request received")
	}
}

func TestRemoteWriteSnapshot(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	src := prometheus.NewRegistry()
	ctr := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_rw_total", Help: "t"}, []string{"code"})
	ctr.WithLabelValues("200").Add(7)
	hist := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_rw_seconds", Help: "t", Buckets: []float64{1}})
	hist.Observe(0.5)
	src.MustRegister(ctr, hist)

	ch := make(chan *prompb.WriteRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "snappy" || r.Header.Get("X-Prometheus-Remote-Write-Version") == "" {
			t.Errorf("headers = %v", r.Header)
		}
		raw, _ := io.ReadAll(r.Body)
		dec, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Errorf("snappy: %v", err)
			w.WriteHeader(400)
			return
		}
		var wr prompb.WriteRequest
		if err := wr.Unmarshal(dec); err != nil {
			t.Errorf("proto: %v", err)
			w.WriteHeader(400)
			return
		}
		select {
		case ch <- &wr:
		default:
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	w := NewRemoteWriter(RemoteWriteConfig{
		URL:      srv.URL,
		Interval: 20 * time.Millisecond,
		Gatherer: src,
		Metrics:  m,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	select {
	case wr := <-ch:
		series := map[string]float64{}
		for _, ts := range wr.Timeseries {
			key := ""
			for _, l := range ts.Labels {
				key += fmt.Sprintf("%s=%s,", l.Name, l.Value)
			}
			series[key] = ts.Samples[0].Value
		}
		checks := map[string]float64{
			"__name__=test_rw_total,code=200,":         7,
			"__name__=test_rw_seconds_bucket,le=1,":    1,
			"__name__=test_rw_seconds_count,":          1,
			"__name__=test_rw_seconds_bucket,le=+Inf,": 1,
		}
		for k, want := range checks {
			if got, ok := series[k]; !ok || got != want {
				t.Errorf("series %q = %v (present=%v), want %v\nall: %v", k, got, ok, want, series)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no write request received")
	}
}
