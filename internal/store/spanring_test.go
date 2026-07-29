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
	"fmt"
	"testing"
	"time"

	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func spanReq(ns string, names []string, ts time.Time) *colltracepb.ExportTraceServiceRequest {
	spans := make([]*tracepb.Span, len(names))
	for i, n := range names {
		spans[i] = &tracepb.Span{
			Name:              n,
			StartTimeUnixNano: uint64(ts.UnixNano()), //nolint:gosec // post-epoch
		}
	}
	return &colltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{{
					Key:   "k8s.namespace.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ns}},
				}},
			},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		}},
	}
}

func TestSpanBufferAppendRange(t *testing.T) {
	b := NewSpanBuffer(8, nil)
	now := time.Now()
	b.AppendRequest(spanReq("shop", []string{"a", "b", "c"}, now))

	if got := b.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	out := b.Range(now.Add(-time.Second), now.Add(time.Second), 0)
	if len(out) != 3 || out[0].Span.GetName() != "a" || out[2].Span.GetName() != "c" {
		t.Fatalf("Range = %+v", out)
	}
	if out[0].Resource["k8s.namespace.name"] != "shop" {
		t.Fatalf("resource attrs = %v", out[0].Resource)
	}
	// Time filter excludes everything.
	if got := b.Range(now.Add(-2*time.Hour), now.Add(-time.Hour), 0); len(got) != 0 {
		t.Fatalf("out-of-window Range = %d entries", len(got))
	}
	// Limit applies.
	if got := b.Range(now.Add(-time.Second), now.Add(time.Second), 2); len(got) != 2 {
		t.Fatalf("limited Range = %d entries", len(got))
	}
}

func TestSpanBufferEviction(t *testing.T) {
	b := NewSpanBuffer(4, nil)
	now := time.Now()
	for i := 0; i < 6; i++ {
		b.AppendRequest(spanReq("ns", []string{fmt.Sprintf("s%d", i)}, now))
	}
	if got := b.Len(); got != 4 {
		t.Fatalf("Len = %d, want 4 (capacity)", got)
	}
	out := b.Range(now.Add(-time.Second), now.Add(time.Second), 0)
	if len(out) != 4 || out[0].Span.GetName() != "s2" || out[3].Span.GetName() != "s5" {
		names := make([]string, len(out))
		for i, s := range out {
			names[i] = s.Span.GetName()
		}
		t.Fatalf("Range after eviction = %v, want [s2 s3 s4 s5]", names)
	}
}

func TestSpanBufferSubscribe(t *testing.T) {
	b := NewSpanBuffer(64, nil)
	now := time.Now()

	ch, cancel := b.Subscribe(t.Context(), 2)
	defer cancel()

	b.AppendRequest(spanReq("ns", []string{"live1"}, now))
	item := <-ch
	if item.Span.GetName() != "live1" || item.Gap != 0 {
		t.Fatalf("item = %+v", item)
	}

	// Overflow the 2-slot buffer without draining: extra spans drop
	// and surface as Gap on the next delivery after draining.
	b.AppendRequest(spanReq("ns", []string{"x1", "x2", "x3", "x4"}, now))
	if got := (<-ch).Span.GetName(); got != "x1" {
		t.Fatalf("first buffered = %s", got)
	}
	if got := (<-ch).Span.GetName(); got != "x2" {
		t.Fatalf("second buffered = %s", got)
	}
	// x3, x4 were dropped (buffer full). The next append delivers
	// with Gap=2.
	b.AppendRequest(spanReq("ns", []string{"after"}, now))
	item = <-ch
	if item.Span.GetName() != "after" || item.Gap != 2 {
		t.Fatalf("post-gap item = name %s gap %d, want after/2", item.Span.GetName(), item.Gap)
	}

	// Cancel closes the channel and stops delivery.
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed after cancel")
	}
	b.AppendRequest(spanReq("ns", []string{"ignored"}, now)) // must not panic
}

func BenchmarkSpanBufferAppend(b *testing.B) {
	buf := NewSpanBuffer(65536, nil)
	req := spanReq("bench", []string{"s"}, time.Now())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.AppendRequest(req)
	}
}
