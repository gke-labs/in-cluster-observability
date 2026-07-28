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

package otlpreceiver

import (
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"testing"

	colllogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Regression tests for the #154 HTTP decoding fixes.

type countingHandler struct{ metrics, traces, logs int }

func (c *countingHandler) OnMetrics(context.Context, *collmetricspb.ExportMetricsServiceRequest) error {
	c.metrics++
	return nil
}
func (c *countingHandler) OnTraces(context.Context, *colltracepb.ExportTraceServiceRequest) error {
	c.traces++
	return nil
}
func (c *countingHandler) OnLogs(context.Context, *colllogspb.ExportLogsServiceRequest) error {
	c.logs++
	return nil
}

func startHTTPOnly(t *testing.T, h Handler) (*Server, string) {
	t.Helper()
	s, err := New(Config{HTTPAddr: "127.0.0.1:0", Handler: h})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	_, addr := s.Addrs()
	return s, "http://" + addr
}

// TestHTTPJSONWithCharset: "application/json; charset=utf-8" must hit
// the JSON decoder. Before #154 the exact-string match fell through to
// protobuf and returned 400.
func TestHTTPJSONWithCharset(t *testing.T) {
	h := &countingHandler{}
	_, base := startHTTPOnly(t, h)

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/metrics", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if h.metrics != 1 {
		t.Fatalf("handler metrics calls = %d, want 1", h.metrics)
	}
}

// TestHTTPGzipBody: OTLP/HTTP permits Content-Encoding: gzip; an OBI
// image that enables exporter compression must not get a 400 (#154).
func TestHTTPGzipBody(t *testing.T) {
	h := &countingHandler{}
	_, base := startHTTPOnly(t, h)

	payload, err := proto.Marshal(&colltracepb.ExportTraceServiceRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/traces", &buf)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if h.traces != 1 {
		t.Fatalf("handler traces calls = %d, want 1", h.traces)
	}
}

// TestPartialStartReleasesGRPC: when the HTTP listen fails after the
// gRPC listener bound, Start must tear the gRPC server down instead of
// leaking a serving goroutine + port (#154).
func TestPartialStartReleasesGRPC(t *testing.T) {
	// Occupy a port for HTTP so its listen fails.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer blocker.Close()

	// Reserve a distinct port for gRPC, then free it for the server.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcAddr := probe.Addr().String()
	probe.Close()

	s, err := New(Config{
		GRPCAddr: grpcAddr,
		HTTPAddr: blocker.Addr().String(),
		Handler:  &countingHandler{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatalf("Start succeeded with occupied HTTP port; want error")
	}

	// The gRPC port must be free again.
	l, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		t.Fatalf("gRPC port still held after failed Start: %v", err)
	}
	l.Close()
}
