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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"

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

func (s *queryServer) Query(ctx context.Context, req *pb.QueryRequest) (*pb.QueryResponse, error) {
	results, err := s.writer.Query(ctx, req.Query)
	if err != nil {
		return nil, err
	}

	var rawResults [][]byte
	for _, res := range results {
		b, err := protojson.Marshal(res)
		if err != nil {
			log.Printf("error marshaling result: %v", err)
			continue
		}
		rawResults = append(rawResults, b)
	}

	return &pb.QueryResponse{Results: rawResults}, nil
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
	path := flag.String("path", "otel-data.bin", "path to the output file")
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

	// Start HTTP server for queries
	mux := http.NewServeMux()
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
