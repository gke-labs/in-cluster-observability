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
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"

	"github.com/gke-labs/in-cluster-observability/internal/store"
)

// RemoteWriteConfig configures the Prometheus remote-write v1
// exporter (ADR-0026 §6: rides the OTLP relay design, fed by the
// same gathered-sample stream as the tsdb ingester).
type RemoteWriteConfig struct {
	URL      string
	Headers  map[string]string
	Interval time.Duration // gather cadence; defaults to 15s
	Timeout  time.Duration // per-request; defaults to 10s
	Gatherer prometheus.Gatherer

	Metrics *Metrics
	Logger  *slog.Logger
}

// RemoteWriter periodically snapshots the registry and pushes a
// WriteRequest through a Relay (same bounded-queue/backoff/drop
// semantics as the OTLP relays).
type RemoteWriter struct {
	cfg   RemoteWriteConfig
	relay *Relay[*prompb.WriteRequest]
}

// NewRemoteWriter builds the exporter.
func NewRemoteWriter(cfg RemoteWriteConfig) *RemoteWriter {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	client := &http.Client{Timeout: cfg.Timeout}
	send := func(ctx context.Context, wr *prompb.WriteRequest) error {
		// prompb is gogo-generated; use its own marshaler.
		raw, err := wr.Marshal()
		if err != nil {
			return Permanent{Err: err}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(snappy.Encode(nil, raw)))
		if err != nil {
			return Permanent{Err: err}
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "snappy")
		req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
		for k, v := range cfg.Headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		switch {
		case resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return fmt.Errorf("remote write %s: %s", cfg.URL, resp.Status)
		default:
			// 4xx: the payload will not become acceptable by retrying.
			return Permanent{Err: fmt.Errorf("remote write %s: %s", cfg.URL, resp.Status)}
		}
	}
	return &RemoteWriter{
		cfg:   cfg,
		relay: NewRelay(cfg.URL, 64, cfg.Metrics, cfg.Logger, send),
	}
}

// Run gathers on the interval and delivers until ctx ends.
func (w *RemoteWriter) Run(ctx context.Context) {
	go w.relay.Run(ctx)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if wr := w.snapshot(); len(wr.Timeseries) > 0 {
				w.relay.Enqueue(wr)
			}
		}
	}
}

// snapshot renders the current registry state as one WriteRequest,
// producing exactly the series a scrape (and the tsdb ingester)
// would: same Flatten, same label synthesis.
func (w *RemoteWriter) snapshot() *prompb.WriteRequest {
	fams, err := w.cfg.Gatherer.Gather()
	if err != nil && fams == nil {
		w.cfg.Logger.Warn("remote write: gather failed", "err", err)
		return &prompb.WriteRequest{}
	}
	ts := time.Now().UnixMilli()
	wr := &prompb.WriteRequest{}
	for _, fam := range fams {
		for _, m := range fam.GetMetric() {
			for _, smp := range store.Flatten(fam, m) {
				lbls := make([]prompb.Label, 0, len(m.GetLabel())+2)
				lbls = append(lbls, prompb.Label{Name: "__name__", Value: smp.Name})
				for _, lp := range m.GetLabel() {
					lbls = append(lbls, prompb.Label{Name: lp.GetName(), Value: lp.GetValue()})
				}
				if smp.Le != "" {
					lbls = append(lbls, prompb.Label{Name: "le", Value: smp.Le})
				}
				if smp.Quantile != "" {
					lbls = append(lbls, prompb.Label{Name: "quantile", Value: smp.Quantile})
				}
				sort.Slice(lbls, func(i, j int) bool { return lbls[i].Name < lbls[j].Name })
				wr.Timeseries = append(wr.Timeseries, prompb.TimeSeries{
					Labels:  lbls,
					Samples: []prompb.Sample{{Value: smp.Value, Timestamp: ts}},
				})
			}
		}
	}
	return wr
}
