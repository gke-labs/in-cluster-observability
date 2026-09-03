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

package queryserver

import (
	"log"

	pkgpb "github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

type SinkQueryServer struct {
	pkgpb.UnimplementedQueryServiceServer
	Querier SinkQuerier
}

func NewSinkQueryServer(querier SinkQuerier) *SinkQueryServer {
	return &SinkQueryServer{
		Querier: querier,
	}
}

func (s *SinkQueryServer) SearchLogs(req *pkgpb.SearchLogsRequest, stream grpc.ServerStreamingServer[pkgpb.SearchLogsResponse]) error {
	results, err := s.Querier.SearchLogs(stream.Context(), req)
	if err != nil {
		return err
	}

	if len(results) > 0 {
		if err := stream.Send(&pkgpb.SearchLogsResponse{Logs: results}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SinkQueryServer) Query(req *pkgpb.QueryRequest, stream grpc.ServerStreamingServer[pkgpb.QueryResponse]) error {
	results, err := s.Querier.Query(stream.Context(), req.Query)
	if err != nil {
		return err
	}

	for _, res := range results {
		switch msg := res.(type) {
		case *colmetricspb.ExportMetricsServiceRequest:
			for _, rm := range msg.ResourceMetrics {
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						singleReq := &colmetricspb.ExportMetricsServiceRequest{
							ResourceMetrics: []*metricspb.ResourceMetrics{
								{
									Resource:  rm.Resource,
									SchemaUrl: rm.SchemaUrl,
									ScopeMetrics: []*metricspb.ScopeMetrics{
										{
											Scope:     sm.Scope,
											SchemaUrl: sm.SchemaUrl,
											Metrics:   []*metricspb.Metric{m},
										},
									},
								},
							},
						}
						b, err := proto.Marshal(singleReq)
						if err != nil {
							log.Printf("error marshaling metric: %v", err)
							continue
						}
						if len(b) > 4194304 {
							txt, _ := prototext.Marshal(singleReq)
							log.Printf("metric message exceeds gRPC max size (4MB). size=%d, msg=%s", len(b), txt)
							continue
						}
						if err := stream.Send(&pkgpb.QueryResponse{Metrics: [][]byte{b}}); err != nil {
							return err
						}
					}
				}
			}
		case *collogspb.ExportLogsServiceRequest:
			for _, rl := range msg.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						singleReq := &collogspb.ExportLogsServiceRequest{
							ResourceLogs: []*logspb.ResourceLogs{
								{
									Resource:  rl.Resource,
									SchemaUrl: rl.SchemaUrl,
									ScopeLogs: []*logspb.ScopeLogs{
										{
											Scope:      sl.Scope,
											SchemaUrl:  sl.SchemaUrl,
											LogRecords: []*logspb.LogRecord{lr},
										},
									},
								},
							},
						}
						b, err := proto.Marshal(singleReq)
						if err != nil {
							log.Printf("error marshaling log: %v", err)
							continue
						}
						if len(b) > 4194304 {
							txt, _ := prototext.Marshal(singleReq)
							log.Printf("log message exceeds gRPC max size (4MB). size=%d, msg=%s", len(b), txt)
							continue
						}
						if err := stream.Send(&pkgpb.QueryResponse{Logs: [][]byte{b}}); err != nil {
							return err
						}
					}
				}
			}
		case *coltracepb.ExportTraceServiceRequest:
			for _, rs := range msg.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, span := range ss.Spans {
						singleReq := &coltracepb.ExportTraceServiceRequest{
							ResourceSpans: []*tracepb.ResourceSpans{
								{
									Resource:  rs.Resource,
									SchemaUrl: rs.SchemaUrl,
									ScopeSpans: []*tracepb.ScopeSpans{
										{
											Scope:     ss.Scope,
											SchemaUrl: ss.SchemaUrl,
											Spans:     []*tracepb.Span{span},
										},
									},
								},
							},
						}
						b, err := proto.Marshal(singleReq)
						if err != nil {
							log.Printf("error marshaling trace: %v", err)
							continue
						}
						if len(b) > 4194304 {
							txt, _ := prototext.Marshal(singleReq)
							log.Printf("trace message exceeds gRPC max size (4MB). size=%d, msg=%s", len(b), txt)
							continue
						}
						if err := stream.Send(&pkgpb.QueryResponse{Traces: [][]byte{b}}); err != nil {
							return err
						}
					}
				}
			}
		default:
			log.Printf("unknown result type: %T", res)
			continue
		}
	}

	return nil
}
