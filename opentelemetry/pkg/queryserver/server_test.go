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
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	pkgpb "github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
)

func TestDiscoveryRedirect(t *testing.T) {
	s := &Server{
		Registry: NewRegistry(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/apis", s.ApisHandler)
	mux.HandleFunc("/apis/", s.ApisHandler)

	tests := []struct {
		path         string
		expectedCode int
	}{
		{"/apis", http.StatusOK},
		{"/apis/", http.StatusOK},
		{"/apis/custom.metrics.k8s.io/v1beta1", http.StatusOK},
		{"/apis/custom.metrics.k8s.io/v1beta1/", http.StatusOK},
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", tt.path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != tt.expectedCode {
			t.Errorf("Path %s: expected %d, got %d", tt.path, tt.expectedCode, rr.Code)
			continue
		}

		if tt.path == "/apis/custom.metrics.k8s.io/v1beta1" {
			var resp map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Errorf("Path %s: failed to unmarshal JSON: %v", tt.path, err)
				continue
			}
			if resp["kind"] != "APIResourceList" {
				t.Errorf("Path %s: expected kind APIResourceList, got %v", tt.path, resp["kind"])
			}
		}
		t.Logf("Path %s: got expected %d", tt.path, rr.Code)
	}
}

type fakeQueryServiceServer struct {
	pkgpb.UnimplementedQueryServiceServer
	logsToReturn [][]byte
}

func (f *fakeQueryServiceServer) SearchLogs(req *pkgpb.SearchLogsRequest, stream grpc.ServerStreamingServer[pkgpb.SearchLogsResponse]) error {
	if len(f.logsToReturn) > 0 {
		if err := stream.Send(&pkgpb.SearchLogsResponse{Logs: f.logsToReturn}); err != nil {
			return err
		}
	}
	return nil
}

func TestQueryServer_SearchLogs(t *testing.T) {
	// 1. Start a fake gRPC QueryService server on a random port
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on TCP: %v", err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	fakeSink := &fakeQueryServiceServer{}
	pkgpb.RegisterQueryServiceServer(grpcServer, fakeSink)

	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	// 2. Prepare some dummy logs to return from fake sink
	logRecord := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{
							Key: "k8s.namespace.name",
							Value: &commonpb.AnyValue{
								Value: &commonpb.AnyValue_StringValue{StringValue: "kube-system"},
							},
						},
						{
							Key: "k8s.pod.name",
							Value: &commonpb.AnyValue{
								Value: &commonpb.AnyValue_StringValue{StringValue: "coredns-test"},
							},
						},
					},
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						LogRecords: []*logspb.LogRecord{
							{
								TimeUnixNano: 1600000000000,
								SeverityText: "ERROR",
								Body: &commonpb.AnyValue{
									Value: &commonpb.AnyValue_StringValue{StringValue: "database connection refused"},
								},
							},
						},
					},
				},
			},
		},
	}
	logBytes, err := proto.Marshal(logRecord)
	if err != nil {
		t.Fatalf("failed to marshal dummy log: %v", err)
	}
	fakeSink.logsToReturn = [][]byte{logBytes}

	// 3. Set up the query server Server with the fake sink's address registered
	s := &Server{
		Registry: &Registry{
			addresses: map[string]int{
				lis.Addr().String(): 1,
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/logs/search", s.SearchLogsHandler)

	// 4. Send search request to query server
	req := httptest.NewRequest("GET", "/api/logs/search?q=connection&start=2026-01-01T00:00:00Z&end=2026-12-31T23:59:59Z&limit=10", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	// 5. Unmarshal and verify the JSON rows response
	var results []SearchResultItem
	if err := json.Unmarshal(rr.Body.Bytes(), &results); err != nil {
		t.Fatalf("failed to unmarshal JSON response: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	res := results[0]
	if res.Severity != "ERROR" {
		t.Errorf("expected severity ERROR, got %q", res.Severity)
	}
	if res.Namespace != "kube-system" {
		t.Errorf("expected namespace kube-system, got %q", res.Namespace)
	}
	if res.Pod != "coredns-test" {
		t.Errorf("expected pod coredns-test, got %q", res.Pod)
	}
	if res.Body != "database connection refused" {
		t.Errorf("expected body %q, got %q", "database connection refused", res.Body)
	}

	// Verify the 'raw' field contains the valid OTLP JSON string
	var rawLogs collogspb.ExportLogsServiceRequest
	if err := json.Unmarshal(res.Raw, &rawLogs); err != nil {
		t.Errorf("raw field is not a valid JSON representation of ExportLogsServiceRequest: %v", err)
	}
}
