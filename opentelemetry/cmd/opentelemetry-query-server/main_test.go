package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoveryRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis", apisHandler)
	mux.HandleFunc("/apis/", apisHandler)

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
