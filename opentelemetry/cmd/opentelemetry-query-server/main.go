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

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	flag.Parse()

	registry := &Registry{
		addresses: make(map[string]bool),
	}

	http.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		registry.Register(req.Address)
		w.WriteHeader(http.StatusAccepted)
	})

	http.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var qreq QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&qreq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		addresses := registry.GetAddresses()
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

		resp := QueryResponse{Results: allResults}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("error encoding response: %v", err)
		}
	})

	// Kubernetes API discovery
	http.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
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
	})

	http.HandleFunc("/apis/custom.metrics.k8s.io", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"kind":             "APIGroup",
			"apiVersion":       "v1",
			"name":             "custom.metrics.k8s.io",
			"versions":         []map[string]any{{"groupVersion": "custom.metrics.k8s.io/v1beta1", "version": "v1beta1"}},
			"preferredVersion": map[string]string{"groupVersion": "custom.metrics.k8s.io/v1beta1", "version": "v1beta1"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/apis/custom.metrics.k8s.io/v1beta1/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Custom metrics query v1beta1: %s", r.URL.Path)
		if r.URL.Path == "/apis/custom.metrics.k8s.io/v1beta1" || r.URL.Path == "/apis/custom.metrics.k8s.io/v1beta1/" {
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

		// Implement the actual metric query handler
		// Format: /apis/custom.metrics.k8s.io/v1beta1/namespaces/{namespace}/pods/{pod-name}/test_metric
		parts := strings.Split(r.URL.Path, "/")
		var namespace, podName string
		if len(parts) >= 8 {
			namespace = parts[5]
			podName = parts[7]
		}

		items := []map[string]any{}
		if podName == "*" {
			// Return a few example pods to trigger scaling
			for i := 0; i < 1; i++ {
				name := "test-app"
				if i > 0 {
					name = fmt.Sprintf("test-app-%d", i)
				}
				items = append(items, map[string]any{
					"describedObject": map[string]string{
						"kind":       "Pod",
						"namespace":  namespace,
						"name":       name,
						"apiVersion": "v1",
					},
					"metricName": "test_metric",
					"timestamp":  time.Now().Format(time.RFC3339),
					"value":      "100", // 100 > target 50
				})
			}
		} else {
			items = append(items, map[string]any{
				"describedObject": map[string]string{
					"kind":       "Pod",
					"namespace":  namespace,
					"name":       podName,
					"apiVersion": "v1",
				},
				"metricName": "test_metric",
				"timestamp":  time.Now().Format(time.RFC3339),
				"value":      "100",
			})
		}

		resp := map[string]any{
			"kind":       "MetricValueList",
			"apiVersion": "custom.metrics.k8s.io/v1beta1",
			"metadata":   map[string]string{"selfLink": r.URL.Path},
			"items":      items,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	log.Printf("query-server listening on %s", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("failed to listen: %v", err)
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
