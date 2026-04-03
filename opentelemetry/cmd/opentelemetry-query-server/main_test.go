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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoveryRedirect(t *testing.T) {
	s := &Server{
		registry: &Registry{
			addresses: make(map[string]bool),
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/apis", s.apisHandler)
	mux.HandleFunc("/apis/", s.apisHandler)

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
