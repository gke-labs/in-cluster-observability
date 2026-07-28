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

// Package otlpreceiver implements the gRPC and HTTP OTLP receivers
// the agent runs on loopback to consume telemetry from the sibling
// OBI container (per ADR-0018). Receivers do not translate to
// capture.Event themselves — they invoke a Handler callback supplied
// at construction time, which pkg/capture wires to its own translator.
//
// Loopback enforcement: the Server's Listen helper rejects any
// non-loopback address. Callers that wire raw listeners themselves are
// responsible for matching that policy.
package otlpreceiver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	colllogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

// Handler receives OTLP messages translated from the gRPC and HTTP
// surfaces. All methods are called synchronously from the receiver
// goroutine — implementations must not block.
//
// Stability: Experimental
type Handler interface {
	OnMetrics(ctx context.Context, req *collmetricspb.ExportMetricsServiceRequest) error
	OnTraces(ctx context.Context, req *colltracepb.ExportTraceServiceRequest) error
	OnLogs(ctx context.Context, req *colllogspb.ExportLogsServiceRequest) error
}

// Config governs Server construction.
//
// Stability: Experimental
type Config struct {
	// GRPCAddr is the bind address for the OTLP gRPC receiver.
	// Must resolve to a loopback IP (127.0.0.0/8 or ::1). Empty disables.
	GRPCAddr string
	// HTTPAddr is the bind address for the OTLP HTTP receiver.
	// Must resolve to a loopback IP. Empty disables.
	HTTPAddr string
	// Handler is invoked per incoming OTLP message.
	Handler Handler
}

// Server runs the gRPC and HTTP OTLP receivers. Construct with New;
// drive via Start / Stop.
//
// Stability: Experimental
type Server struct {
	cfg Config

	mu         sync.Mutex
	started    bool
	stopped    bool
	grpcServer *grpc.Server
	httpServer *http.Server
	grpcAddr   string
	httpAddr   string
	wg         sync.WaitGroup
}

// New constructs a Server. Returns an error if Config has no addresses
// or no handler.
//
// Stability: Experimental
func New(cfg Config) (*Server, error) {
	if cfg.Handler == nil {
		return nil, errors.New("otlpreceiver: Config.Handler is required")
	}
	if cfg.GRPCAddr == "" && cfg.HTTPAddr == "" {
		return nil, errors.New("otlpreceiver: at least one of GRPCAddr or HTTPAddr must be set")
	}
	if err := requireLoopback(cfg.GRPCAddr); err != nil {
		return nil, fmt.Errorf("otlpreceiver: GRPCAddr: %w", err)
	}
	if err := requireLoopback(cfg.HTTPAddr); err != nil {
		return nil, fmt.Errorf("otlpreceiver: HTTPAddr: %w", err)
	}
	return &Server{cfg: cfg}, nil
}

// Addrs returns the resolved gRPC and HTTP listen addresses once the
// Server is Started. Useful for tests using ephemeral ports.
func (s *Server) Addrs() (grpcAddr, httpAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Computed at Start.
	return s.grpcAddr, s.httpAddr
}

// Start binds listeners and begins serving. Returns immediately; the
// underlying servers run on background goroutines. Idempotent: a
// second Start is a no-op error.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("otlpreceiver: already started")
	}
	if s.stopped {
		s.mu.Unlock()
		return errors.New("otlpreceiver: server already stopped")
	}
	s.started = true
	s.mu.Unlock()

	if s.cfg.GRPCAddr != "" {
		l, err := net.Listen("tcp", s.cfg.GRPCAddr)
		if err != nil {
			return fmt.Errorf("otlpreceiver: gRPC listen %q: %w", s.cfg.GRPCAddr, err)
		}
		s.mu.Lock()
		s.grpcAddr = l.Addr().String()
		s.mu.Unlock()
		gs := grpc.NewServer()
		registerGRPC(gs, s.cfg.Handler)
		s.mu.Lock()
		s.grpcServer = gs
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = gs.Serve(l)
		}()
	}

	if s.cfg.HTTPAddr != "" {
		l, err := net.Listen("tcp", s.cfg.HTTPAddr)
		if err != nil {
			// Don't leak a half-started server: the gRPC listener may
			// already be serving on its own goroutine (#154).
			s.mu.Lock()
			gs := s.grpcServer
			s.mu.Unlock()
			if gs != nil {
				gs.Stop()
				s.wg.Wait()
			}
			return fmt.Errorf("otlpreceiver: HTTP listen %q: %w", s.cfg.HTTPAddr, err)
		}
		s.mu.Lock()
		s.httpAddr = l.Addr().String()
		s.mu.Unlock()
		mux := http.NewServeMux()
		registerHTTP(mux, s.cfg.Handler)
		hs := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		s.mu.Lock()
		s.httpServer = hs
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = hs.Serve(l)
		}()
	}

	return nil
}

// Stop gracefully shuts down both servers and waits for them to exit.
// Idempotent.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	gs, hs := s.grpcServer, s.httpServer
	s.mu.Unlock()

	if gs != nil {
		gs.GracefulStop()
	}
	if hs != nil {
		_ = hs.Shutdown(ctx)
	}
	s.wg.Wait()
	return nil
}
