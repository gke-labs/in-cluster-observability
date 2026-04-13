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

package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
)

type traceServer struct {
	writer *Writer
	coltracepb.UnimplementedTraceServiceServer
}

func (s *traceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if err := s.writer.WriteObject(ctx, req); err != nil {
		log.Printf("error writing traces: %v", err)
		return nil, err
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type metricsServer struct {
	writer *Writer
	colmetricspb.UnimplementedMetricsServiceServer
}

func (s *metricsServer) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	if err := s.writer.WriteObject(ctx, req); err != nil {
		log.Printf("error writing metrics: %v", err)
		return nil, err
	}
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

type logsServer struct {
	writer *Writer
	collogspb.UnimplementedLogsServiceServer
}

func (s *logsServer) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if err := s.writer.WriteObject(ctx, req); err != nil {
		log.Printf("error writing logs: %v", err)
		return nil, err
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

type queryServer struct {
	writer *Writer
	pb.UnimplementedQueryServiceServer
}

func (s *queryServer) Query(req *pb.QueryRequest, stream grpc.ServerStreamingServer[pb.QueryResponse]) error {
	results, err := s.writer.Query(stream.Context(), req.Query)
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
									Resource:     rm.Resource,
									SchemaUrl:    rm.SchemaUrl,
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
						if err := stream.Send(&pb.QueryResponse{Metrics: [][]byte{b}}); err != nil {
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
						if err := stream.Send(&pb.QueryResponse{Logs: [][]byte{b}}); err != nil {
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
									Resource:   rs.Resource,
									SchemaUrl:  rs.SchemaUrl,
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
						if err := stream.Send(&pb.QueryResponse{Traces: [][]byte{b}}); err != nil {
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

type QueryRequest struct {
	Query string `json:"query"`
}

type QueryResponse struct {
	Results []json.RawMessage `json:"results"`
}

func main() {
	addr := flag.String("addr", ":4317", "address to listen on for gRPC")
	httpAddr := flag.String("http-addr", ":4318", "address to listen on for HTTP queries")
	queryServerAddr := flag.String("query-server", "queryserver.observability-system:9443", "address of the query server")
	path := flag.String("path", "otel-data", "path to the output directory")
	flag.Parse()

	writer, err := NewWriter(*path)
	if err != nil {
		log.Fatalf("failed to create writer: %v", err)
	}
	defer writer.Close()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(s, &traceServer{writer: writer})
	colmetricspb.RegisterMetricsServiceServer(s, &metricsServer{writer: writer})
	collogspb.RegisterLogsServiceServer(s, &logsServer{writer: writer})
	pb.RegisterQueryServiceServer(s, &queryServer{writer: writer})

	log.Printf("gRPC listening on %s, writing to %s", *addr, *path)

	go func() {
		if err := s.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	// Start HTTP server for queries and OTLP collection
	mux := http.NewServeMux()

	handleOTLP := func(w http.ResponseWriter, r *http.Request, req proto.Message, resp proto.Message) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		contentType := r.Header.Get("Content-Type")
		if contentType == "application/json" {
			err = protojson.Unmarshal(body, req)
		} else {
			err = proto.Unmarshal(body, req)
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := writer.WriteObject(r.Context(), req); err != nil {
			log.Printf("error writing object: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		accept := r.Header.Get("Accept")
		var respBody []byte
		if accept == "application/json" {
			respBody, _ = protojson.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
		} else {
			respBody, _ = proto.Marshal(resp)
			w.Header().Set("Content-Type", "application/x-protobuf")
		}
		w.Write(respBody)
	}

	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		handleOTLP(w, r, &coltracepb.ExportTraceServiceRequest{}, &coltracepb.ExportTraceServiceResponse{})
	})
	mux.HandleFunc("/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		handleOTLP(w, r, &colmetricspb.ExportMetricsServiceRequest{}, &colmetricspb.ExportMetricsServiceResponse{})
	})
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		handleOTLP(w, r, &collogspb.ExportLogsServiceRequest{}, &collogspb.ExportLogsServiceResponse{})
	})

	mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var qreq QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&qreq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		results, err := writer.Query(r.Context(), qreq.Query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		var rawResults []json.RawMessage
		for _, res := range results {
			b, err := protojson.Marshal(res)
			if err != nil {
				log.Printf("error marshaling result: %v", err)
				continue
			}
			rawResults = append(rawResults, json.RawMessage(b))
		}

		resp := QueryResponse{Results: rawResults}
		json.NewEncoder(w).Encode(resp)
	})

	log.Printf("HTTP listening on %s", *httpAddr)
	go func() {
		if err := http.ListenAndServe(*httpAddr, mux); err != nil {
			log.Fatalf("failed to serve http: %v", err)
		}
	}()

	// Register with query server
	go func() {
		podIP := os.Getenv("POD_IP")
		if podIP == "" {
			log.Println("POD_IP not set, skipping registration")
			return
		}
		// Use gRPC port for querying the sink
		_, port, _ := net.SplitHostPort(*addr)
		myAddr := net.JoinHostPort(podIP, port)

		for {
			func() {
				conn, err := grpc.NewClient(*queryServerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					log.Printf("failed to connect to query server %s: %v", *queryServerAddr, err)
					return
				}
				defer conn.Close()

				client := pb.NewRegistrationServiceClient(conn)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				stream, err := client.Register(ctx)
				if err != nil {
					log.Printf("failed to open registration stream to %s: %v", *queryServerAddr, err)
					return
				}

				log.Printf("successfully opened registration stream to query server at %s", *queryServerAddr)

				for {
					req := &pb.RegisterRequest{Address: myAddr}
					if err := stream.Send(req); err != nil {
						log.Printf("failed to send registration heartbeat: %v", err)
						return
					}

					// Wait for acknowledgment (or heartbeat response)
					_, err := stream.Recv()
					if err != nil {
						log.Printf("registration stream closed: %v", err)
						return
					}
					time.Sleep(5 * time.Second)
				}
			}()
			time.Sleep(5 * time.Second)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down")
	s.GracefulStop()
}
