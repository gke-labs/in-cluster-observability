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

package export

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// OTLPConfig configures the OTLP relay pair (#97 gRPC, #98 HTTP).
type OTLPConfig struct {
	// Endpoint: host:port for gRPC; a base URL (http://host:port)
	// for HTTP.
	Endpoint string
	// Protocol is "grpc" or "http".
	Protocol string
	// Headers are attached to every export (auth tokens etc.).
	Headers map[string]string
	// Compression: "gzip" or "" (none).
	Compression string
	// Timeout bounds one delivery attempt. Defaults to 10s.
	Timeout time.Duration
	// Buffer is the per-relay queue bound. Defaults to 1024.
	Buffer int

	Metrics *Metrics
	Logger  *slog.Logger
}

// OTLPRelays is the metrics+traces relay pair sharing one endpoint.
type OTLPRelays struct {
	Metrics *Relay[*collmetricspb.ExportMetricsServiceRequest]
	Traces  *Relay[*colltracepb.ExportTraceServiceRequest]
}

// Run starts both workers and blocks until ctx ends.
func (o *OTLPRelays) Run(ctx context.Context) {
	go o.Metrics.Run(ctx)
	o.Traces.Run(ctx)
}

// NewOTLPRelays builds the relay pair for cfg.
func NewOTLPRelays(cfg OTLPConfig) (*OTLPRelays, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	switch cfg.Protocol {
	case "grpc", "":
		return newOTLPGRPC(cfg)
	case "http":
		return newOTLPHTTP(cfg)
	default:
		return nil, fmt.Errorf("export: unknown OTLP protocol %q (want grpc|http)", cfg.Protocol)
	}
}

// newOTLPGRPC relays over the standard collector gRPC services.
// Transport security is v0.6 (ADR-0025 §7); in-cluster hops are
// plaintext + NetworkPolicy-bounded until then.
func newOTLPGRPC(cfg OTLPConfig) (*OTLPRelays, error) {
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("export: grpc client for %s: %w", cfg.Endpoint, err)
	}
	var callOpts []grpc.CallOption
	if cfg.Compression == "gzip" {
		callOpts = append(callOpts, grpc.UseCompressor(grpcgzip.Name))
	}
	outCtx := func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		for k, v := range cfg.Headers {
			ctx = metadata.AppendToOutgoingContext(ctx, k, v)
		}
		return ctx, cancel
	}

	mc := collmetricspb.NewMetricsServiceClient(conn)
	tc := colltracepb.NewTraceServiceClient(conn)
	return &OTLPRelays{
		Metrics: NewRelay(cfg.Endpoint, cfg.Buffer, cfg.Metrics, cfg.Logger,
			func(ctx context.Context, req *collmetricspb.ExportMetricsServiceRequest) error {
				ctx, cancel := outCtx(ctx)
				defer cancel()
				_, err := mc.Export(ctx, req, callOpts...)
				return err
			}),
		Traces: NewRelay(cfg.Endpoint, cfg.Buffer, cfg.Metrics, cfg.Logger,
			func(ctx context.Context, req *colltracepb.ExportTraceServiceRequest) error {
				ctx, cancel := outCtx(ctx)
				defer cancel()
				_, err := tc.Export(ctx, req, callOpts...)
				return err
			}),
	}, nil
}

func newOTLPHTTP(cfg OTLPConfig) (*OTLPRelays, error) {
	base := strings.TrimSuffix(cfg.Endpoint, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("export: OTLP HTTP endpoint must be a base URL, got %q", cfg.Endpoint)
	}
	client := &http.Client{Timeout: cfg.Timeout}
	send := func(path string) func(ctx context.Context, msg proto.Message) error {
		url := base + path
		return func(ctx context.Context, msg proto.Message) error {
			body, err := proto.Marshal(msg)
			if err != nil {
				return Permanent{Err: err}
			}
			var reader io.Reader = bytes.NewReader(body)
			if cfg.Compression == "gzip" {
				var buf bytes.Buffer
				zw := gzip.NewWriter(&buf)
				if _, err := zw.Write(body); err != nil {
					return Permanent{Err: err}
				}
				if err := zw.Close(); err != nil {
					return Permanent{Err: err}
				}
				reader = &buf
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
			if err != nil {
				return Permanent{Err: err}
			}
			req.Header.Set("Content-Type", "application/x-protobuf")
			if cfg.Compression == "gzip" {
				req.Header.Set("Content-Encoding", "gzip")
			}
			for k, v := range cfg.Headers {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			switch {
			case resp.StatusCode < 300:
				return nil
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				return fmt.Errorf("otlp http %s: %s", url, resp.Status)
			default:
				return Permanent{Err: fmt.Errorf("otlp http %s: %s", url, resp.Status)}
			}
		}
	}
	sendMetrics := send("/v1/metrics")
	sendTraces := send("/v1/traces")
	return &OTLPRelays{
		Metrics: NewRelay(cfg.Endpoint, cfg.Buffer, cfg.Metrics, cfg.Logger,
			func(ctx context.Context, req *collmetricspb.ExportMetricsServiceRequest) error {
				return sendMetrics(ctx, req)
			}),
		Traces: NewRelay(cfg.Endpoint, cfg.Buffer, cfg.Metrics, cfg.Logger,
			func(ctx context.Context, req *colltracepb.ExportTraceServiceRequest) error {
				return sendTraces(ctx, req)
			}),
	}, nil
}
