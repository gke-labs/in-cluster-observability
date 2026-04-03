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
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

type Registry struct {
	mu        sync.Mutex
	addresses map[string]bool
}

func (r *Registry) Register(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addresses[address] = true
	log.Printf("registered sink: %s", address)
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

type RegisterRequest struct {
	Address string `json:"address"`
}

type QueryRequest struct {
	Query string `json:"query"`
}

type QueryResponse struct {
	Results []json.RawMessage `json:"results"`
}

type Server struct {
	registry *Registry
}

func main() {
	addr := flag.String("addr", ":8443", "address to listen on")
	tlsCertFile := flag.String("tls-cert-file", "", "TLS certificate file")
	tlsKeyFile := flag.String("tls-private-key-file", "", "TLS private key file")
	flag.Parse()

	s := &Server{
		registry: &Registry{
			addresses: make(map[string]bool),
		},
	}

	http.HandleFunc("/register", s.registerHandler)
	http.HandleFunc("/query", s.queryHandler)
	http.HandleFunc("/apis", s.apisHandler)
	http.HandleFunc("/apis/", s.apisHandler)

	log.Printf("query-server listening on %s", *addr)
	if *tlsCertFile != "" && *tlsKeyFile != "" {
		if err := http.ListenAndServeTLS(*addr, *tlsCertFile, *tlsKeyFile, nil); err != nil {
			log.Fatalf("failed to listen (TLS): %v", err)
		}
	} else {
		if err := http.ListenAndServe(*addr, nil); err != nil {
			log.Fatalf("failed to listen: %v", err)
		}
	}
}

func (s *Server) registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.registry.Register(req.Address)
	w.WriteHeader(http.StatusAccepted)
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
	var allResults []json.RawMessage

	for _, sinkAddr := range addresses {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			results, err := querySink(addr, qreq)
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

	log.Printf("Query response for %q: %d results", qreq.Query, len(allResults))
	resp := QueryResponse{Results: allResults}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("error encoding response: %v", err)
	}
}

func querySink(addr string, qreq QueryRequest) ([]json.RawMessage, error) {
	data, err := json.Marshal(qreq)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s/query", addr)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sink returned status %d: %s", resp.StatusCode, string(body))
	}

	var qresp QueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&qresp); err != nil {
		return nil, err
	}
	return qresp.Results, nil
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
			Query: fmt.Sprintf("metric=%s;namespace=%s;pod=%s", metricName, namespace, podName),
		}

		addresses := s.registry.GetAddresses()
		var wg sync.WaitGroup
		var mu sync.Mutex
		var allResults []json.RawMessage

		for _, sinkAddr := range addresses {
			wg.Add(1)
			go func(addr string) {
				defer wg.Done()
				results, err := querySink(addr, qreq)
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

		items := []map[string]any{}
		for _, raw := range allResults {
			var mreq colmetricspb.ExportMetricsServiceRequest
			if err := protojson.Unmarshal(raw, &mreq); err != nil {
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
						timestamp := time.Now()

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
							items = append(items, map[string]any{
								"describedObject": map[string]string{
									"kind":       "Pod",
									"namespace":  resNamespace,
									"name":       resPodName,
									"apiVersion": "v1",
								},
								"metricName": metricName,
								"timestamp":  timestamp.Format(time.RFC3339),
								"value":      value,
							})
						}
					}
				}
			}
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
