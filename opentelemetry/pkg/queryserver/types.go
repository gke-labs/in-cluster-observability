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
	"encoding/json"
	"sync"

	pkgpb "github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
	"google.golang.org/protobuf/proto"
)

// SinkQuerier is an interface for querying telemetry and searching logs from a sink.
type SinkQuerier interface {
	Query(ctx context.Context, query string) ([]proto.Message, error)
	SearchLogs(ctx context.Context, req *pkgpb.SearchLogsRequest) ([][]byte, error)
}

type Registry struct {
	mu        sync.Mutex
	addresses map[string]int
}

func NewRegistry() *Registry {
	return &Registry{
		addresses: make(map[string]int),
	}
}

func (r *Registry) Register(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addresses[address]++
}

func (r *Registry) Unregister(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addresses[address]--
	if r.addresses[address] <= 0 {
		delete(r.addresses, address)
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

type SearchResultItem struct {
	Timestamp string          `json:"timestamp"`
	Severity  string          `json:"severity"`
	Namespace string          `json:"namespace"`
	Pod       string          `json:"pod"`
	Container string          `json:"container"`
	Service   string          `json:"service"`
	Body      string          `json:"body"`
	Raw       json.RawMessage `json:"raw"`
}

type MatchedLogItem struct {
	Timestamp int64
	Item      SearchResultItem
}
