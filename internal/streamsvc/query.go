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
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	streamv1 "github.com/gke-labs/in-cluster-observability/pkg/stream/pb/stream/v1"
)

const (
	minStep = time.Second
	maxStep = 5 * time.Minute
)

// Discovery lists the current agent stream endpoints (host:port).
// Implemented by the fanout Discoverer via an adapter in cmd.
type Discovery interface {
	Addrs() []string
}

// Evaluator runs instant PromQL over the fan-out; the second return
// reports degradation. Implemented by queryapi.API.
type Evaluator interface {
	InstantVectorDegraded(ctx context.Context, expr string, ts time.Time) (promql.Vector, bool, error)
}

// QueryServer is the cluster-wide stream surface: span subscriptions
// multiplexed across every discovered agent, and periodic PromQL
// evaluation streams.
type QueryServer struct {
	streamv1.UnimplementedStreamServiceServer

	Agents Discovery
	Eval   Evaluator
	// BearerTokenFile authenticates upstream agent subscriptions
	// (same token the remote-read fan-out presents).
	BearerToken func() string
	Logger      *slog.Logger
}

// SubscribeSpans validates the filter locally (fail fast with a
// clear error), then opens one upstream subscription per agent and
// multiplexes the results. Agents joining after the subscription
// starts are not added retroactively; agents failing mid-stream are
// logged and dropped from the mux while the rest continue.
func (s *QueryServer) SubscribeSpans(req *streamv1.SubscribeSpansRequest, srv streamv1.StreamService_SubscribeSpansServer) error {
	if _, err := CompileFilter(req.GetCelFilter()); err != nil {
		return status.Errorf(codes.InvalidArgument, "cel filter: %v", err)
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	addrs := s.Agents.Addrs()
	if len(addrs) == 0 {
		return status.Error(codes.Unavailable, "no agents discovered")
	}

	ctx, cancel := context.WithCancel(srv.Context())
	defer cancel()

	events := make(chan *streamv1.SpanEvent, 256)
	var live sync.WaitGroup
	for _, addr := range addrs {
		live.Add(1)
		go func(addr string) {
			defer live.Done()
			s.forwardAgent(ctx, addr, req, events, logger)
		}(addr)
	}
	// When the last upstream forwarder exits, the subscription can
	// never produce another span. Ending the stream with a status —
	// instead of hanging open silently (#190) — lets the subscriber
	// distinguish "every agent is gone / rejected us" from "no
	// matching traffic" and resubscribe (picking up current agents).
	allGone := make(chan struct{})
	go func() {
		live.Wait()
		close(allGone)
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-events:
			if err := srv.Send(ev); err != nil {
				return err
			}
		case <-allGone:
			// Drain what the forwarders buffered before they exited.
			for {
				select {
				case ev := <-events:
					if err := srv.Send(ev); err != nil {
						return err
					}
				default:
					return status.Error(codes.Unavailable,
						"all upstream agent streams ended (agents restarted, or every subscription was rejected); resubscribe")
				}
			}
		}
	}
}

func (s *QueryServer) forwardAgent(ctx context.Context, addr string, req *streamv1.SubscribeSpansRequest, out chan<- *streamv1.SpanEvent, logger *slog.Logger) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Warn("stream mux: agent dial failed", "addr", addr, "err", err)
		return
	}
	defer conn.Close()

	if s.BearerToken != nil {
		if tok := s.BearerToken(); tok != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
		}
	}
	stream, err := streamv1.NewStreamServiceClient(conn).SubscribeSpans(ctx, req)
	if err != nil {
		logger.Warn("stream mux: agent subscribe failed", "addr", addr, "err", err)
		return
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("stream mux: agent stream ended", "addr", addr, "err", err)
			}
			return
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// StreamMetrics evaluates the PromQL expression every step and
// streams the samples.
func (s *QueryServer) StreamMetrics(req *streamv1.StreamMetricsRequest, srv streamv1.StreamService_StreamMetricsServer) error {
	if s.Eval == nil {
		return status.Error(codes.Unavailable, "metric streaming unavailable")
	}
	step := time.Duration(req.GetStepMs()) * time.Millisecond
	if step <= 0 {
		step = 15 * time.Second
	}
	if step < minStep {
		step = minStep
	}
	if step > maxStep {
		step = maxStep
	}

	ctx := srv.Context()
	t := time.NewTicker(step)
	defer t.Stop()
	for {
		now := time.Now()
		vec, degraded, err := s.Eval.InstantVectorDegraded(ctx, req.GetPromql(), now)
		if err != nil {
			// Only a parse/type error is the caller's fault (#190):
			// evaluation can also fail transiently (agents mid-roll,
			// deadline pressure), and InvalidArgument tells
			// well-behaved clients to stop retrying. Classify by
			// parsing the expression ourselves.
			if _, perr := parser.NewParser(parser.Options{}).ParseExpr(req.GetPromql()); perr != nil {
				return status.Errorf(codes.InvalidArgument, "promql: %v", perr)
			}
			return status.Errorf(codes.Unavailable, "promql evaluation: %v", err)
		}
		for _, smp := range vec {
			lbls := map[string]string{}
			smp.Metric.Range(func(l labels.Label) { lbls[l.Name] = l.Value })
			if err := srv.Send(&streamv1.MetricSample{
				Labels:      lbls,
				Value:       smp.F,
				TimestampMs: smp.T,
				Degraded:    degraded,
			}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
