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
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/gke-labs/in-cluster-observability/internal/store"
	streamv1 "github.com/gke-labs/in-cluster-observability/pkg/stream/pb/stream/v1"
)

// AgentServer serves node-local span subscriptions from the span
// ring. StreamMetrics is a query-server concern and is unimplemented
// here.
type AgentServer struct {
	streamv1.UnimplementedStreamServiceServer
	Spans  *store.SpanBuffer
	Logger *slog.Logger
}

// SubscribeSpans compiles the CEL filter once, then forwards
// matching live spans until the client goes away.
func (s *AgentServer) SubscribeSpans(req *streamv1.SubscribeSpansRequest, srv streamv1.StreamService_SubscribeSpansServer) error {
	if s.Spans == nil {
		return status.Error(codes.Unavailable, "span ring disabled on this agent")
	}
	filter, err := CompileFilter(req.GetCelFilter())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "cel filter: %v", err)
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ctx := srv.Context()
	ch, cancel := s.Spans.Subscribe(ctx, 1024)
	defer cancel()

	// Filtered-out spans still count into the next delivery's gap?
	// No: gap means "lost to backpressure", not "didn't match" —
	// carry drop-gaps across non-matching spans instead.
	var carriedGap uint64
	for {
		select {
		case <-ctx.Done():
			return nil
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			match, err := filter(item.Span, item.Resource)
			if err != nil {
				// A filter that errors on a real span is a client
				// bug; surface it rather than silently dropping.
				return status.Errorf(codes.InvalidArgument, "cel evaluation: %v", err)
			}
			if !match {
				carriedGap += item.Gap
				continue
			}
			raw, err := proto.Marshal(item.Span)
			if err != nil {
				logger.Warn("stream: span marshal failed", "err", err)
				carriedGap += item.Gap + 1
				continue
			}
			ev := &streamv1.SpanEvent{
				Span:     raw,
				Resource: item.Resource,
				Gap:      item.Gap + carriedGap,
			}
			carriedGap = 0
			if err := srv.Send(ev); err != nil {
				return err
			}
		}
	}
}
