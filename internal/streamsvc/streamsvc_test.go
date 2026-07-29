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

package streamsvc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/gke-labs/in-cluster-observability/internal/store"
	streamv1 "github.com/gke-labs/in-cluster-observability/pkg/stream/pb/stream/v1"
)

func TestCompileFilter(t *testing.T) {
	span := &tracepb.Span{Name: "GET /users", Kind: tracepb.Span_SPAN_KIND_SERVER}
	res := map[string]string{"k8s.namespace.name": "shop"}

	for _, tc := range []struct {
		expr  string
		match bool
	}{
		{"", true},
		{`resource["k8s.namespace.name"] == "shop"`, true},
		{`resource["k8s.namespace.name"] == "payments"`, false},
		{`span.name.startsWith("GET")`, true},
		{`span.kind == 2`, true}, // SPAN_KIND_SERVER
		{`span.name.contains("users") && resource["k8s.namespace.name"] == "shop"`, true},
	} {
		f, err := CompileFilter(tc.expr)
		if err != nil {
			t.Errorf("CompileFilter(%q): %v", tc.expr, err)
			continue
		}
		got, err := f(span, res)
		if err != nil {
			t.Errorf("eval(%q): %v", tc.expr, err)
			continue
		}
		if got != tc.match {
			t.Errorf("filter %q = %v, want %v", tc.expr, got, tc.match)
		}
	}

	// Non-bool and invalid expressions are rejected at compile time.
	if _, err := CompileFilter(`span.name`); err == nil {
		t.Error("non-bool filter accepted")
	}
	if _, err := CompileFilter(`nonsense(((`); err == nil {
		t.Error("invalid filter accepted")
	}
}

func startAgent(t *testing.T, buf *store.SpanBuffer) string {
	t.Helper()
	gs := grpc.NewServer()
	streamv1.RegisterStreamServiceServer(gs, &AgentServer{Spans: buf})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go gs.Serve(l) //nolint:errcheck
	t.Cleanup(gs.Stop)
	return l.Addr().String()
}

func dialStream(t *testing.T, addr string) streamv1.StreamServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return streamv1.NewStreamServiceClient(conn)
}

func appendSpan(buf *store.SpanBuffer, ns, name string) {
	buf.AppendRequest(&colltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "k8s.namespace.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ns}},
			}}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
				Name:              name,
				StartTimeUnixNano: uint64(time.Now().UnixNano()), //nolint:gosec
			}}}},
		}},
	})
}

// TestAgentSubscribeFiltered is #99's core: only matching spans
// reach the subscriber, as decodable OTLP.
func TestAgentSubscribeFiltered(t *testing.T) {
	buf := store.NewSpanBuffer(64, nil)
	addr := startAgent(t, buf)
	client := dialStream(t, addr)

	stream, err := client.SubscribeSpans(t.Context(), &streamv1.SubscribeSpansRequest{
		CelFilter: `resource["k8s.namespace.name"] == "shop"`,
	})
	if err != nil {
		t.Fatalf("SubscribeSpans: %v", err)
	}

	// Let the server register the subscription before appending.
	time.Sleep(200 * time.Millisecond)
	appendSpan(buf, "payments", "skip-me")
	appendSpan(buf, "shop", "keep-me")

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	var span tracepb.Span
	if err := proto.Unmarshal(ev.GetSpan(), &span); err != nil {
		t.Fatalf("span decode: %v", err)
	}
	if span.GetName() != "keep-me" || ev.GetResource()["k8s.namespace.name"] != "shop" {
		t.Fatalf("got span %q res %v", span.GetName(), ev.GetResource())
	}
}

// TestAgentSubscribeBadFilter: compile errors surface as
// InvalidArgument on the stream, not as silent empties.
func TestAgentSubscribeBadFilter(t *testing.T) {
	buf := store.NewSpanBuffer(64, nil)
	addr := startAgent(t, buf)
	client := dialStream(t, addr)

	stream, err := client.SubscribeSpans(t.Context(), &streamv1.SubscribeSpansRequest{CelFilter: `((`})
	if err == nil {
		if _, err = stream.Recv(); err == nil {
			t.Fatal("bad filter accepted")
		}
	}
}

// TestQueryServerMux: spans from two agents arrive on one stream.
func TestQueryServerMux(t *testing.T) {
	buf1 := store.NewSpanBuffer(64, nil)
	buf2 := store.NewSpanBuffer(64, nil)
	a1 := startAgent(t, buf1)
	a2 := startAgent(t, buf2)

	qs := &QueryServer{Agents: staticAddrs{a1, a2}}
	gs := grpc.NewServer()
	streamv1.RegisterStreamServiceServer(gs, qs)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go gs.Serve(l) //nolint:errcheck
	t.Cleanup(gs.Stop)

	client := dialStream(t, l.Addr().String())
	stream, err := client.SubscribeSpans(t.Context(), &streamv1.SubscribeSpansRequest{})
	if err != nil {
		t.Fatalf("SubscribeSpans: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	appendSpan(buf1, "ns", "from-agent-1")
	appendSpan(buf2, "ns", "from-agent-2")

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
		var span tracepb.Span
		_ = proto.Unmarshal(ev.GetSpan(), &span)
		got[span.GetName()] = true
	}
	if !got["from-agent-1"] || !got["from-agent-2"] {
		t.Fatalf("mux missed an agent: %v", got)
	}
}

type staticAddrs []string

func (s staticAddrs) Addrs() []string { return s }

type fakeEval struct{ vec promql.Vector }

func (f fakeEval) InstantVectorDegraded(context.Context, string, time.Time) (promql.Vector, bool, error) {
	return f.vec, true, nil
}

// TestStreamMetrics: periodic evaluation streams samples with the
// degraded flag.
func TestStreamMetrics(t *testing.T) {
	qs := &QueryServer{
		Agents: staticAddrs{},
		Eval: fakeEval{vec: promql.Vector{{
			Metric: labels.FromStrings("__name__", "test_up", "k8s_pod_name", "p1"),
			F:      1, T: 42,
		}}},
	}
	gs := grpc.NewServer()
	streamv1.RegisterStreamServiceServer(gs, qs)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go gs.Serve(l) //nolint:errcheck
	t.Cleanup(gs.Stop)

	client := dialStream(t, l.Addr().String())
	stream, err := client.StreamMetrics(t.Context(), &streamv1.StreamMetricsRequest{Promql: "test_up", StepMs: 1000})
	if err != nil {
		t.Fatalf("StreamMetrics: %v", err)
	}
	smp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if smp.GetLabels()["__name__"] != "test_up" || smp.GetValue() != 1 || !smp.GetDegraded() {
		t.Fatalf("sample = %+v", smp)
	}
}
