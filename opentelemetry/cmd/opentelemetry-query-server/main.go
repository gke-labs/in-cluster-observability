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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"

	"github.com/google/cel-go/cel"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

type Registry struct {
	mu        sync.Mutex
	addresses map[string]int
}

func (r *Registry) Register(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addresses[address]++
	log.Printf("registered sink: %s (active registrations: %d)", address, r.addresses[address])
}

func (r *Registry) Unregister(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addresses[address]--
	if r.addresses[address] <= 0 {
		delete(r.addresses, address)
		log.Printf("unregistered sink: %s", address)
	} else {
		log.Printf("decreased registration count for sink: %s (active: %d)", address, r.addresses[address])
	}
}

func (r *Registry) GetAddresses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var addrs []string
	for addr := range r.addresses {
		addrs = append(addrs, addr)
	}
	return addrs
}

type QueryRequest struct {
	Query string `json:"query"`
}

type QueryResponse struct {
	Results []json.RawMessage `json:"results"`
}

type Server struct {
	registry *Registry
	pb.UnimplementedRegistrationServiceServer
	pb.UnimplementedFrontendQueryServiceServer
}

func (s *Server) Register(stream pb.RegistrationService_RegisterServer) error {
	var address string
	defer func() {
		if address != "" {
			s.registry.Unregister(address)
		}
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		if address == "" {
			address = req.Address
			s.registry.Register(address)
		} else if address != req.Address {
			s.registry.Unregister(address)
			address = req.Address
			s.registry.Register(address)
		}

		if err := stream.Send(&pb.RegisterResponse{}); err != nil {
			return err
		}
	}
}

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

	s := &Server{
		registry: &Registry{
			addresses: make(map[string]int),
		},
	}

	http.HandleFunc("/query", s.queryHandler)
	http.HandleFunc("/apis", s.apisHandler)
	http.HandleFunc("/apis/", s.apisHandler)

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

	handler := otelhttp.NewHandler(http.DefaultServeMux, "query-server")
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

func (s *Server) queryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var qreq QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&qreq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("Received query: %s", qreq.Query)

	addresses := s.registry.GetAddresses()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allResults [][]byte

	for _, sinkAddr := range addresses {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			results, err := querySink(r.Context(), addr, qreq)
			if err != nil {
				log.Printf("error querying sink %s: %v", addr, err)
				return
			}
			mu.Lock()
			allResults = append(allResults, results...)
			mu.Unlock()
		}(sinkAddr)
	}
	wg.Wait()

	var rawResults []json.RawMessage
	for _, raw := range allResults {
		var msg proto.Message
		var mLog collogspb.ExportLogsServiceRequest
		var mMetric colmetricspb.ExportMetricsServiceRequest
		var mTrace coltracepb.ExportTraceServiceRequest

		if err := proto.Unmarshal(raw, &mLog); err == nil && len(mLog.ResourceLogs) > 0 {
			msg = &mLog
		} else if err := proto.Unmarshal(raw, &mMetric); err == nil && len(mMetric.ResourceMetrics) > 0 {
			msg = &mMetric
		} else if err := proto.Unmarshal(raw, &mTrace); err == nil && len(mTrace.ResourceSpans) > 0 {
			msg = &mTrace
		} else {
			continue
		}

		b, err := protojson.Marshal(msg)
		if err == nil {
			rawResults = append(rawResults, json.RawMessage(b))
		}
	}

	log.Printf("Query response for %q: %d results", qreq.Query, len(rawResults))
	resp := QueryResponse{Results: rawResults}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("error encoding response: %v", err)
	}
}

func querySink(ctx context.Context, addr string, qreq QueryRequest) ([][]byte, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := pb.NewQueryServiceClient(conn)
	stream, err := client.Query(ctx, &pb.QueryRequest{Query: qreq.Query})
	if err != nil {
		return nil, err
	}

	var results [][]byte
	for {
		res, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		results = append(results, res.Metrics...)
		results = append(results, res.Logs...)
		results = append(results, res.Traces...)
	}
	return results, nil
}

func (s *Server) apisHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/apis" || path == "/apis/" {
		resp := map[string]any{
			"kind": "APIGroupList",
			"groups": []map[string]any{
				{
					"name": "custom.metrics.k8s.io",
					"versions": []map[string]any{
						{
							"groupVersion": "custom.metrics.k8s.io/v1beta1",
							"version":      "v1beta1",
						},
					},
					"preferredVersion": map[string]any{
						"groupVersion": "custom.metrics.k8s.io/v1beta1",
						"version":      "v1beta1",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if path == "/apis/custom.metrics.k8s.io" || path == "/apis/custom.metrics.k8s.io/" {
		resp := map[string]any{
			"kind":             "APIGroup",
			"apiVersion":       "v1",
			"name":             "custom.metrics.k8s.io",
			"versions":         []map[string]any{{"groupVersion": "custom.metrics.k8s.io/v1beta1", "version": "v1beta1"}},
			"preferredVersion": map[string]string{"groupVersion": "custom.metrics.k8s.io/v1beta1", "version": "v1beta1"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if path == "/apis/custom.metrics.k8s.io/v1beta1" || path == "/apis/custom.metrics.k8s.io/v1beta1/" {
		log.Printf("Custom metrics query v1beta1: %s", r.URL.Path)
		resp := map[string]any{
			"kind":         "APIResourceList",
			"apiVersion":   "v1",
			"groupVersion": "custom.metrics.k8s.io/v1beta1",
			"resources": []map[string]any{
				{
					"name":         "pods/test_metric",
					"singularName": "",
					"namespaced":   true,
					"kind":         "MetricValueList",
					"verbs":        []string{"get"},
				},
				{
					"name":         "pods/qps",
					"singularName": "",
					"namespaced":   true,
					"kind":         "MetricValueList",
					"verbs":        []string{"get"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if strings.HasPrefix(path, "/apis/custom.metrics.k8s.io/v1beta1/") {
		log.Printf("Custom metrics query v1beta1: %s", r.URL.Path)
		// Implement the actual metric query handler
		// Format: /apis/custom.metrics.k8s.io/v1beta1/namespaces/{namespace}/pods/{pod-name}/{metric-name}
		parts := strings.Split(strings.TrimPrefix(path, "/apis/custom.metrics.k8s.io/v1beta1/"), "/")
		var namespace, podName, metricName string
		if len(parts) >= 4 && parts[0] == "namespaces" && parts[2] == "pods" {
			namespace = parts[1]
			podName = parts[3]
			metricName = parts[len(parts)-1]
		} else {
			http.Error(w, "invalid path format", http.StatusBadRequest)
			return
		}

		qreq := QueryRequest{
			Query: fmt.Sprintf("metric=%s;namespace=%s;pod=%s;latest_only=true", metricName, namespace, podName),
		}

		addresses := s.registry.GetAddresses()
		var wg sync.WaitGroup
		var mu sync.Mutex
		var allResults [][]byte

		for _, sinkAddr := range addresses {
			wg.Add(1)
			go func(addr string) {
				defer wg.Done()
				results, err := querySink(r.Context(), addr, qreq)
				if err != nil {
					log.Printf("error querying sink %s: %v", addr, err)
					return
				}
				mu.Lock()
				allResults = append(allResults, results...)
				mu.Unlock()
			}(sinkAddr)
		}
		wg.Wait()

		type podKey struct {
			namespace string
			podName   string
		}
		latestItems := make(map[podKey]struct {
			item      map[string]any
			timestamp time.Time
		})

		for _, raw := range allResults {
			var mreq colmetricspb.ExportMetricsServiceRequest
			if err := proto.Unmarshal(raw, &mreq); err != nil {
				log.Printf("failed to unmarshal OTLP metrics: %v", err)
				continue
			}

			for _, rm := range mreq.ResourceMetrics {
				// Re-verify namespace and pod name from resource attributes
				var resPodName, resNamespace string
				for _, attr := range rm.Resource.Attributes {
					if attr.Key == "k8s.pod.name" {
						resPodName = attr.Value.GetStringValue()
					} else if attr.Key == "k8s.namespace.name" {
						resNamespace = attr.Value.GetStringValue()
					}
				}

				if namespace != "" && resNamespace != namespace {
					continue
				}
				if podName != "" && podName != "*" && resPodName != podName {
					continue
				}

				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						if m.Name != metricName {
							continue
						}

						// Extract value from the latest data point
						value := ""
						timestamp := time.Time{}

						if sum := m.GetSum(); sum != nil {
							if len(sum.DataPoints) > 0 {
								dp := sum.DataPoints[len(sum.DataPoints)-1]
								value = fmt.Sprintf("%v", dp.GetAsInt())
								if dp.GetAsDouble() != 0 {
									value = fmt.Sprintf("%v", dp.GetAsDouble())
								}
								timestamp = time.Unix(0, int64(dp.TimeUnixNano))
							}
						} else if gauge := m.GetGauge(); gauge != nil {
							if len(gauge.DataPoints) > 0 {
								dp := gauge.DataPoints[len(gauge.DataPoints)-1]
								value = fmt.Sprintf("%v", dp.GetAsInt())
								if dp.GetAsDouble() != 0 {
									value = fmt.Sprintf("%v", dp.GetAsDouble())
								}
								timestamp = time.Unix(0, int64(dp.TimeUnixNano))
							}
						}

						if value != "" {
							key := podKey{namespace: resNamespace, podName: resPodName}
							if existing, ok := latestItems[key]; !ok || timestamp.After(existing.timestamp) {
								latestItems[key] = struct {
									item      map[string]any
									timestamp time.Time
								}{
									item: map[string]any{
										"describedObject": map[string]string{
											"kind":       "Pod",
											"namespace":  resNamespace,
											"name":       resPodName,
											"apiVersion": "v1",
										},
										"metricName": metricName,
										"timestamp":  timestamp.Format(time.RFC3339),
										"value":      value,
									},
									timestamp: timestamp,
								}
							}
						}
					}
				}
			}
		}

		items := []map[string]any{}
		for _, v := range latestItems {
			items = append(items, v.item)
		}

		// Log the query and response
		log.Printf("APIS Query: %s -> %d items", qreq.Query, len(items))

		resp := map[string]any{
			"kind":       "MetricValueList",
			"apiVersion": "custom.metrics.k8s.io/v1beta1",
			"metadata":   map[string]string{"selfLink": path},
			"items":      items,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) Query(ctx context.Context, req *pb.FrontendQueryRequest) (*pb.FrontendQueryResponse, error) {
	// 1. Fetch ALL data from all sinks. We send an empty query string.
	qreq := QueryRequest{Query: ""}
	addresses := s.registry.GetAddresses()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var allResults [][]byte

	for _, sinkAddr := range addresses {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			results, err := querySink(ctx, addr, qreq)
			if err != nil {
				log.Printf("error querying sink %s: %v", addr, err)
				return
			}
			mu.Lock()
			allResults = append(allResults, results...)
			mu.Unlock()
		}(sinkAddr)
	}
	wg.Wait()

	// 2. Prepare CEL environments
	var envOpts []cel.EnvOption
	// TODO: Maybe we should wrap the protobuf types to avoid a dependency on a proto we do not control (and then we would use ext.NativeTypes).
	envOpts = append(envOpts, cel.Types(
		&collogspb.ExportLogsServiceRequest{},
		&colmetricspb.ExportMetricsServiceRequest{},
		&coltracepb.ExportTraceServiceRequest{},
	))

	var varName string
	switch req.Table {
	case pb.Table_LOGS:
		varName = "log"
		envOpts = append(envOpts, cel.Variable(varName, cel.ObjectType("opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest")))
	case pb.Table_TRACES:
		varName = "trace"
		envOpts = append(envOpts, cel.Variable(varName, cel.ObjectType("opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest")))
	case pb.Table_METRICS:
		varName = "metric"
		envOpts = append(envOpts, cel.Variable(varName, cel.ObjectType("opentelemetry.proto.collector.metrics.v1.ExportMetricsServiceRequest")))
	default:
		return nil, fmt.Errorf("unsupported table type: %v", req.Table)
	}

	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL env: %v", err)
	}

	// Compile filters
	var programs []cel.Program
	for _, f := range req.Filters {
		ast, iss := env.Compile(f)
		if iss.Err() != nil {
			return nil, fmt.Errorf("failed to compile filter %q: %v", f, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("failed to create program for filter %q: %v", f, err)
		}
		programs = append(programs, prg)
	}

	// 3. Evaluate filters on results
	var filteredResults [][]byte
	for _, raw := range allResults {
		var msg proto.Message
		var err error

		switch req.Table {
		case pb.Table_LOGS:
			var m collogspb.ExportLogsServiceRequest
			err = proto.Unmarshal(raw, &m)
			msg = &m
		case pb.Table_TRACES:
			var m coltracepb.ExportTraceServiceRequest
			err = proto.Unmarshal(raw, &m)
			msg = &m
		case pb.Table_METRICS:
			var m colmetricspb.ExportMetricsServiceRequest
			err = proto.Unmarshal(raw, &m)
			msg = &m
		}

		if err != nil {
			// Not all returned objects match the requested type, just skip them.
			continue
		}

		match := true
		for _, prg := range programs {
			out, _, err := prg.Eval(map[string]any{
				varName: msg,
			})
			if err != nil {
				// Evaluation error means it doesn't match
				match = false
				break
			}
			if out.Value() != true {
				match = false
				break
			}
		}

		if match {
			b, err := protojson.Marshal(msg)
			if err == nil {
				filteredResults = append(filteredResults, b)
			}
		}
	}

	return &pb.FrontendQueryResponse{Results: filteredResults}, nil
}
