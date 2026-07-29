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

// Package export implements the agent's push egress (#97, #98,
// ADR-0026 §6): relays that forward the ORIGINAL OTLP payloads from
// the capture bridge's raw tee to operator-configured endpoints, and
// a Prometheus remote-write v1 exporter fed by the same
// gathered-sample stream the tsdb ingester uses.
//
// The invariant carried over from the superseded in-process sink
// design (sinks-and-extensibility.md §6): egress never blocks or
// takes down capture. Bounded per-endpoint queues, drop-on-full with
// accounting, exponential backoff, permanent errors dropped.
package export

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the shared ollie_export_* instrument set
// (sinks-and-extensibility.md §7); one instance covers all relays.
type Metrics struct {
	Batches  *prometheus.CounterVec // endpoint
	Dropped  *prometheus.CounterVec // endpoint, reason
	Errors   *prometheus.CounterVec // endpoint, kind
	Duration *prometheus.HistogramVec
}

// NewMetrics registers the set on reg (nil skips registration).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Batches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollie_export_batches_total",
			Help: "Batches successfully delivered, by endpoint.",
		}, []string{"endpoint"}),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollie_export_dropped_total",
			Help: "Batches dropped, by endpoint and reason.",
		}, []string{"endpoint", "reason"}),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollie_export_errors_total",
			Help: "Delivery errors, by endpoint and kind (transient|permanent).",
		}, []string{"endpoint", "kind"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ollie_export_duration_seconds",
			Help:    "Per-batch delivery latency, by endpoint.",
			Buckets: prometheus.DefBuckets,
		}, []string{"endpoint"}),
	}
	if reg != nil {
		reg.MustRegister(m.Batches, m.Dropped, m.Errors, m.Duration)
	}
	return m
}

// Permanent marks an error as non-retriable (e.g. HTTP 4xx): the
// batch is dropped immediately with kind=permanent accounting.
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }
func (p Permanent) Unwrap() error { return p.Err }

// Relay is one bounded queue + delivery worker for payload type T.
type Relay[T any] struct {
	endpoint string
	ch       chan T
	send     func(ctx context.Context, item T) error
	metrics  *Metrics
	logger   *slog.Logger

	// Backoff schedule: initial*2^n capped at max; maxRetries
	// attempts per batch before dropping (retry_exhausted).
	initialBackoff time.Duration
	maxBackoff     time.Duration
	maxRetries     int
}

// NewRelay builds a relay. buffer defaults to 1024 (the §4 bound);
// send delivers one item and may return Permanent to skip retries.
func NewRelay[T any](endpoint string, buffer int, m *Metrics, logger *slog.Logger, send func(ctx context.Context, item T) error) *Relay[T] {
	if buffer <= 0 {
		buffer = 1024
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Relay[T]{
		endpoint:       endpoint,
		ch:             make(chan T, buffer),
		send:           send,
		metrics:        m,
		logger:         logger,
		initialBackoff: 500 * time.Millisecond,
		maxBackoff:     30 * time.Second,
		maxRetries:     3,
	}
}

// Enqueue hands a batch to the relay without blocking; a full queue
// drops the batch (buffer_full).
func (r *Relay[T]) Enqueue(item T) {
	select {
	case r.ch <- item:
	default:
		r.metrics.Dropped.WithLabelValues(r.endpoint, "buffer_full").Inc()
	}
}

// Run delivers until ctx ends. Blocking; run in a goroutine.
func (r *Relay[T]) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-r.ch:
			r.deliver(ctx, item)
		}
	}
}

func (r *Relay[T]) deliver(ctx context.Context, item T) {
	backoff := r.initialBackoff
	for attempt := 0; ; attempt++ {
		start := time.Now()
		err := r.send(ctx, item)
		r.metrics.Duration.WithLabelValues(r.endpoint).Observe(time.Since(start).Seconds())
		if err == nil {
			r.metrics.Batches.WithLabelValues(r.endpoint).Inc()
			return
		}
		var perm Permanent
		if errors.As(err, &perm) {
			r.metrics.Errors.WithLabelValues(r.endpoint, "permanent").Inc()
			r.metrics.Dropped.WithLabelValues(r.endpoint, "permanent").Inc()
			r.logger.Warn("export: permanent delivery failure; batch dropped", "endpoint", r.endpoint, "err", err)
			return
		}
		r.metrics.Errors.WithLabelValues(r.endpoint, "transient").Inc()
		if attempt+1 >= r.maxRetries {
			r.metrics.Dropped.WithLabelValues(r.endpoint, "retry_exhausted").Inc()
			r.logger.Warn("export: retries exhausted; batch dropped", "endpoint", r.endpoint, "attempts", attempt+1, "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > r.maxBackoff {
			backoff = r.maxBackoff
		}
	}
}
