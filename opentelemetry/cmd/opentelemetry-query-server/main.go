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
	"net/http"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/queryserver"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
)

func main() {
	addr := flag.String("addr", ":8443", "address to listen on")
	grpcAddr := flag.String("grpc-addr", ":9443", "gRPC address to listen on for registrations")
	tlsCertFile := flag.String("tls-cert-file", "", "TLS certificate file")
	tlsKeyFile := flag.String("tls-private-key-file", "", "TLS private key file")
	flag.Parse()

	shutdown, err := initOtel(context.Background())
	if err != nil {
		log.Printf("failed to initialize OpenTelemetry: %v", err)
	} else {
		defer func() {
			_ = shutdown(context.Background())
		}()
	}

	registry := queryserver.NewRegistry()
	s := queryserver.NewServer(registry)

	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	// Start gRPC server for registrations
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("failed to listen on gRPC: %v", err)
	}
	gs := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	pb.RegisterRegistrationServiceServer(gs, s)
	pb.RegisterFrontendQueryServiceServer(gs, s)
	log.Printf("gRPC server listening on %s", *grpcAddr)
	go func() {
		if err := gs.Serve(lis); err != nil {
			log.Fatalf("failed to serve gRPC: %v", err)
		}
	}()

	handler := otelhttp.NewHandler(mux, "query-server")
	log.Printf("query-server listening on %s", *addr)
	if *tlsCertFile != "" && *tlsKeyFile != "" {
		if err := http.ListenAndServeTLS(*addr, *tlsCertFile, *tlsKeyFile, handler); err != nil {
			log.Fatalf("failed to listen (TLS): %v", err)
		}
	} else {
		if err := http.ListenAndServe(*addr, handler); err != nil {
			log.Fatalf("failed to listen: %v", err)
		}
	}
}
