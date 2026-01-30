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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestMetricsEndpoint(t *testing.T) {
	// Initialize some metrics
	updateMetrics()
	tcpConnections.WithLabelValues("outbound").Add(0)
	tcpConnections.WithLabelValues("inbound").Add(0)
	httpRequests.WithLabelValues("GET").Add(0)

	req, err := http.NewRequest("GET", "/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := promhttp.Handler()

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	// Check if we have some expected metrics in the output
	body := rr.Body.String()
	expectedMetrics := []string{
		"node_network_receive_bytes_total",
		"node_network_transmit_bytes_total",
		"node_tcp_connections_total",
		"node_http_requests_total",
	}

	for _, metric := range expectedMetrics {
		if !contains(body, metric) {
			t.Errorf("metric %s not found in output", metric)
		}
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
