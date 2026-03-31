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
	"sync"
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
