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
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// StoredSpan is one ring entry: the raw OTLP span plus its resource
// attributes flattened to strings (ADR-0026 §5 — CEL filters and
// stream payloads use real OTLP shapes, zero translation loss).
type StoredSpan struct {
	Resource map[string]string
	Span     *tracepb.Span
}

// StreamItem is one live-subscription delivery. Gap counts spans
// dropped for THIS subscriber since its previous delivery
// (drop-oldest semantics made visible, sinks-and-extensibility.md §6).
type StreamItem struct {
	StoredSpan
	Gap uint64
}

// SpanBuffer is the node-local span ring (#84): a fixed-capacity
// in-memory window with ad-hoc Range reads and live Subscribe
// fan-out. No WAL by design — see ADR-0026 §5.
type SpanBuffer struct {
	mu   sync.RWMutex
	buf  []StoredSpan
	head int // next write index
	full bool

	subs map[*subscriber]struct{}

	entries prometheus.GaugeFunc
	drops   *prometheus.CounterVec
}

type subscriber struct {
	ch  chan StreamItem
	gap atomic.Uint64
}

// NewSpanBuffer builds a ring with the given capacity (default
// 65536) and registers its self-obs metrics on reg when non-nil.
func NewSpanBuffer(capacity int, reg prometheus.Registerer) *SpanBuffer {
	if capacity <= 0 {
		capacity = 65536
	}
	b := &SpanBuffer{
		buf:  make([]StoredSpan, capacity),
		subs: map[*subscriber]struct{}{},
		drops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollie_store_ring_drops_total",
			Help: "Ring entries dropped, by ring and reason (capacity eviction or slow subscriber).",
		}, []string{"ring", "reason"}),
	}
	b.entries = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "ollie_store_ring_entries",
		Help:        "Entries currently held, by ring.",
		ConstLabels: prometheus.Labels{"ring": "spans"},
	}, func() float64 { return float64(b.Len()) })
	if reg != nil {
		reg.MustRegister(b.entries, b.drops)
	}
	return b
}

// Len reports the current entry count.
func (b *SpanBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.full {
		return len(b.buf)
	}
	return b.head
}

// AppendRequest flattens an OTLP export request into ring entries
// and fans each out to live subscribers. Called from the capture
// bridge's raw tee; must not block.
func (b *SpanBuffer) AppendRequest(req *colltracepb.ExportTraceServiceRequest) {
	for _, rs := range req.GetResourceSpans() {
		res := flattenAttrs(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				b.append(StoredSpan{Resource: res, Span: span})
			}
		}
	}
}

func (b *SpanBuffer) append(s StoredSpan) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		b.drops.WithLabelValues("spans", "capacity").Inc()
	}
	b.buf[b.head] = s
	b.head = (b.head + 1) % len(b.buf)
	if b.head == 0 {
		b.full = true
	}
	// Deliver under the lock: sends are non-blocking, and Subscribe's
	// cancel path takes the write lock before closing a channel, so a
	// send can never race a close.
	for sub := range b.subs {
		item := StreamItem{StoredSpan: s, Gap: sub.gap.Swap(0)}
		select {
		case sub.ch <- item:
			continue
		default:
		}
		// Slow consumer, channel full: evict the OLDEST queued
		// delivery to make room for the new span (#187). A live tail
		// must converge on the freshest spans — dropping the newest
		// (the previous behavior) meant a stalled subscriber came
		// back to a buffer of stale spans and never saw current
		// traffic, the inverse of the drop-oldest contract in
		// ADR-0026 §7 and stream/v1. The evicted delivery and its
		// accumulated gap fold into this delivery's gap marker.
		select {
		case old := <-sub.ch:
			item.Gap += old.Gap + 1
			b.drops.WithLabelValues("spans", "slow_subscriber").Inc()
		default:
			// Consumer drained concurrently; there's room again.
		}
		select {
		case sub.ch <- item:
		default:
			// Only reachable with a zero-capacity channel: restore
			// the gap for the next attempt.
			sub.gap.Add(item.Gap + 1)
			b.drops.WithLabelValues("spans", "slow_subscriber").Inc()
		}
	}
}

// Range returns up to limit spans whose start time falls in
// [min, max], oldest first. limit <= 0 means no limit.
func (b *SpanBuffer) Range(min, max time.Time, limit int) []StoredSpan {
	minNs, maxNs := uint64(min.UnixNano()), uint64(max.UnixNano()) //nolint:gosec // times are post-epoch
	b.mu.RLock()
	defer b.mu.RUnlock()

	n := b.head
	start := 0
	if b.full {
		n = len(b.buf)
		start = b.head
	}
	var out []StoredSpan
	for i := 0; i < n; i++ {
		s := b.buf[(start+i)%len(b.buf)]
		ts := s.Span.GetStartTimeUnixNano()
		if ts < minNs || ts > maxNs {
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Subscribe registers a live consumer with its own buffer. The
// returned channel closes when ctx ends or cancel is called. A
// consumer that falls behind loses the oldest undelivered spans and
// sees the loss as Gap on its next delivery.
func (b *SpanBuffer) Subscribe(ctx context.Context, buffer int) (<-chan StreamItem, func()) {
	if buffer <= 0 {
		buffer = 1024
	}
	sub := &subscriber{ch: make(chan StreamItem, buffer)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			// Close under the write lock so append's in-lock sends
			// can never hit a closed channel.
			b.mu.Lock()
			delete(b.subs, sub)
			close(sub.ch)
			b.mu.Unlock()
		})
	}
	if ctx != nil {
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}
	return sub.ch, cancel
}

// flattenAttrs renders OTLP resource attributes as a string map for
// CEL and stream payloads.
func flattenAttrs(attrs []*commonpb.KeyValue) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		out[kv.GetKey()] = anyValueString(kv.GetValue())
	}
	return out
}

func anyValueString(v *commonpb.AnyValue) string {
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		if val.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	default:
		return v.String()
	}
}
