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
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/parser"
	pkgpb "github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
	"github.com/google/cel-go/cel"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	pkgpb.UnimplementedRegistrationServiceServer
	pkgpb.UnimplementedFrontendQueryServiceServer
	Registry *Registry
}

func NewServer(registry *Registry) *Server {
	return &Server{
		Registry: registry,
	}
}

func (s *Server) Register(stream pkgpb.RegistrationService_RegisterServer) error {
	var address string
	defer func() {
		if address != "" {
			s.Registry.Unregister(address)
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
			s.Registry.Register(address)
		} else if address != req.Address {
			s.Registry.Unregister(address)
			address = req.Address
			s.Registry.Register(address)
		}

		if err := stream.Send(&pkgpb.RegisterResponse{}); err != nil {
			return err
		}
	}
}

func (s *Server) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/query", s.QueryHandler)
	mux.HandleFunc("/api/logs/search", s.SearchLogsHandler)
	mux.HandleFunc("/apis", s.ApisHandler)
	mux.HandleFunc("/apis/", s.ApisHandler)

	subFS, err := fs.Sub(uiFS, "ui")
	if err != nil {
		log.Fatalf("failed to open embedded UI filesystem: %v", err)
	}
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(subFS))))
}

func (s *Server) QueryHandler(w http.ResponseWriter, r *http.Request) {
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

	addresses := s.Registry.GetAddresses()
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

	client := pkgpb.NewQueryServiceClient(conn)
	stream, err := client.Query(ctx, &pkgpb.QueryRequest{Query: qreq.Query})
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

func (s *Server) SearchLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query().Get("q")
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	limitStr := r.URL.Query().Get("limit")

	var start, end time.Time
	var err error

	if startStr != "" {
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid start time format (expected RFC3339): %v", err), http.StatusBadRequest)
			return
		}
	} else {
		start = time.Now().Add(-1 * time.Hour)
	}

	if endStr != "" {
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid end time format (expected RFC3339): %v", err), http.StatusBadRequest)
			return
		}
	} else {
		end = time.Now()
	}

	limit := 1000
	if limitStr != "" {
		if val, err := strconv.Atoi(limitStr); err == nil && val > 0 {
			limit = val
		}
	}

	parsedQuery := parser.Parse(q)

	var attributes []*pkgpb.AttributeFilter
	for k, v := range parsedQuery.Attributes {
		attributes = append(attributes, &pkgpb.AttributeFilter{
			Key:   k,
			Value: v,
		})
	}

	req := &pkgpb.SearchLogsRequest{
		StartTimeUnixNano: start.UnixNano(),
		EndTimeUnixNano:   end.UnixNano(),
		BodyContains:      parsedQuery.BodyContains,
		Attributes:        attributes,
		Limit:             int32(limit),
	}

	addresses := s.Registry.GetAddresses()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allSerializedLogs [][]byte

	for _, addr := range addresses {
		wg.Add(1)
		go func(sinkAddr string) {
			defer wg.Done()
			res, err := searchLogsFromSink(r.Context(), sinkAddr, req)
			if err != nil {
				log.Printf("error searching logs from sink %s: %v", sinkAddr, err)
				return
			}
			mu.Lock()
			allSerializedLogs = append(allSerializedLogs, res...)
			mu.Unlock()
		}(addr)
	}
	wg.Wait()

	var matchedItems []MatchedLogItem

	for _, rawBytes := range allSerializedLogs {
		var unmarshaled collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(rawBytes, &unmarshaled); err != nil {
			log.Printf("error unmarshaling log search result: %v", err)
			continue
		}

		if len(unmarshaled.ResourceLogs) == 0 ||
			len(unmarshaled.ResourceLogs[0].ScopeLogs) == 0 ||
			len(unmarshaled.ResourceLogs[0].ScopeLogs[0].LogRecords) == 0 {
			continue
		}

		rl := unmarshaled.ResourceLogs[0]
		lr := rl.ScopeLogs[0].LogRecords[0]

		ts := lr.TimeUnixNano
		if ts == 0 {
			ts = lr.ObservedTimeUnixNano
		}

		var ns, pod, container, service string
		if rl.Resource != nil {
			for _, attr := range rl.Resource.Attributes {
				switch attr.Key {
				case "k8s.namespace.name":
					ns = anyValueString(attr.Value)
				case "k8s.pod.name":
					pod = anyValueString(attr.Value)
				case "k8s.container.name":
					container = anyValueString(attr.Value)
				case "service.name":
					service = anyValueString(attr.Value)
				}
			}
		}

		rawJSON, err := protojson.Marshal(&unmarshaled)
		if err != nil {
			log.Printf("error marshaling log to OTLP JSON: %v", err)
			continue
		}

		timeObj := time.Unix(0, int64(ts)).UTC()

		matchedItems = append(matchedItems, MatchedLogItem{
			Timestamp: int64(ts),
			Item: SearchResultItem{
				Timestamp: timeObj.Format(time.RFC3339Nano),
				Severity:  lr.SeverityText,
				Namespace: ns,
				Pod:       pod,
				Container: container,
				Service:   service,
				Body:      anyValueString(lr.Body),
				Raw:       json.RawMessage(rawJSON),
			},
		})
	}

	sort.Slice(matchedItems, func(i, j int) bool {
		return matchedItems[i].Timestamp > matchedItems[j].Timestamp
	})

	if len(matchedItems) > limit {
		matchedItems = matchedItems[:limit]
	}

	results := []SearchResultItem{}
	for _, mi := range matchedItems {
		results = append(results, mi.Item)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Printf("error encoding search results: %v", err)
	}
}

func searchLogsFromSink(ctx context.Context, addr string, req *pkgpb.SearchLogsRequest) ([][]byte, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := pkgpb.NewQueryServiceClient(conn)
	stream, err := client.SearchLogs(ctx, req)
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
		results = append(results, res.Logs...)
	}
	return results, nil
}

func anyValueString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		if val.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	default:
		return v.String()
	}
}

func (s *Server) ApisHandler(w http.ResponseWriter, r *http.Request) {
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
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("error encoding groups: %v", err)
		}
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
		_ = json.NewEncoder(w).Encode(resp)
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
		_ = json.NewEncoder(w).Encode(resp)
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

		addresses := s.Registry.GetAddresses()
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
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) Query(req *pkgpb.FrontendQueryRequest, stream grpc.ServerStreamingServer[pkgpb.FrontendQueryResponse]) error {
	ctx := stream.Context()
	// 1. Fetch ALL data from all sinks. We send an empty query string.
	qreq := QueryRequest{Query: ""}
	addresses := s.Registry.GetAddresses()

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
	envOpts = append(envOpts, cel.Types(
		&collogspb.ExportLogsServiceRequest{},
		&colmetricspb.ExportMetricsServiceRequest{},
		&coltracepb.ExportTraceServiceRequest{},
	))

	var varName string
	switch req.Table {
	case pkgpb.Table_LOGS:
		varName = "log"
		envOpts = append(envOpts, cel.Variable(varName, cel.ObjectType("opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest")))
	case pkgpb.Table_TRACES:
		varName = "trace"
		envOpts = append(envOpts, cel.Variable(varName, cel.ObjectType("opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest")))
	case pkgpb.Table_METRICS:
		varName = "metric"
		envOpts = append(envOpts, cel.Variable(varName, cel.ObjectType("opentelemetry.proto.collector.metrics.v1.ExportMetricsServiceRequest")))
	default:
		return fmt.Errorf("unsupported table type: %v", req.Table)
	}

	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return fmt.Errorf("failed to create CEL env: %v", err)
	}

	// Compile filters
	var programs []cel.Program
	for _, f := range req.Filters {
		ast, iss := env.Compile(f)
		if iss.Err() != nil {
			return fmt.Errorf("failed to compile filter %q: %v", f, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			return fmt.Errorf("failed to create program for filter %q: %v", f, err)
		}
		programs = append(programs, prg)
	}

	// 3. Evaluate filters on results
	for _, raw := range allResults {
		var msg proto.Message
		var err error

		switch req.Table {
		case pkgpb.Table_LOGS:
			var m collogspb.ExportLogsServiceRequest
			err = proto.Unmarshal(raw, &m)
			msg = &m
		case pkgpb.Table_TRACES:
			var m coltracepb.ExportTraceServiceRequest
			err = proto.Unmarshal(raw, &m)
			msg = &m
		case pkgpb.Table_METRICS:
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
				if err := stream.Send(&pkgpb.FrontendQueryResponse{Results: [][]byte{b}}); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
