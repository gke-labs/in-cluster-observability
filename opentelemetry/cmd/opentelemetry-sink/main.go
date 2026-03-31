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
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
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

func main() {
	addr := flag.String("addr", ":4317", "address to listen on")
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

	log.Printf("listening on %s, writing to %s", *addr, *path)

	go func() {
		if err := s.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down")
	s.GracefulStop()
}
