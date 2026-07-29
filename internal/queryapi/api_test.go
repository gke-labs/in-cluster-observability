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

package queryapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/gke-labs/in-cluster-observability/internal/store"
)

func newAPIOverStore(t *testing.T, ready bool) (*API, *store.Store) {
	t.Helper()
	st, err := store.New(store.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	api := New(Config{
		Queryable: st.Queryable(),
		Ready:     func() bool { return ready },
	})
	return api, st
}

func writeSample(t *testing.T, st *store.Store, name string, ts time.Time, v float64, kv ...string) {
	t.Helper()
	app := st.Appender(t.Context())
	pairs := append([]string{labels.MetricName, name}, kv...)
	if _, err := app.Append(0, labels.FromStrings(pairs...), ts.UnixMilli(), v); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func get(t *testing.T, h http.Handler, path string) (int, envelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env envelope
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, env
}

func TestInstantQuery(t *testing.T) {
	api, st := newAPIOverStore(t, true)
	now := time.Now().Truncate(time.Second)
	writeSample(t, st, "test_up", now, 1, "k8s_pod_name", "p1")

	code, env := get(t, api.Routes(),
		fmt.Sprintf("/api/v1/query?query=%s&time=%d", url.QueryEscape(`test_up`), now.Unix()))
	if code != http.StatusOK || env.Status != "success" {
		t.Fatalf("code=%d env=%+v", code, env)
	}
	if env.Data.ResultType != "vector" {
		t.Fatalf("resultType = %s, want vector", env.Data.ResultType)
	}
	var result []struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	if err := json.Unmarshal(env.Data.Result, &result); err != nil {
		t.Fatalf("result unmarshal: %v", err)
	}
	if len(result) != 1 || result[0].Metric["k8s_pod_name"] != "p1" || result[0].Value[1] != "1" {
		t.Fatalf("result = %+v", result)
	}
	if env.Degraded {
		t.Fatal("unexpected degraded flag")
	}
}

func TestRangeQuery(t *testing.T) {
	api, st := newAPIOverStore(t, true)
	base := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	for i := 0; i < 60; i++ {
		writeSample(t, st, "test_counter_total", base.Add(time.Duration(i)*time.Second), float64(i))
	}
	end := base.Add(59 * time.Second)

	code, env := get(t, api.Routes(), fmt.Sprintf(
		"/api/v1/query_range?query=%s&start=%d&end=%d&step=15",
		url.QueryEscape(`rate(test_counter_total[30s])`), base.Add(30*time.Second).Unix(), end.Unix()))
	if code != http.StatusOK || env.Status != "success" {
		t.Fatalf("code=%d env=%+v", code, env)
	}
	if env.Data.ResultType != "matrix" {
		t.Fatalf("resultType = %s, want matrix", env.Data.ResultType)
	}
	var result []struct {
		Values [][]any `json:"values"`
	}
	if err := json.Unmarshal(env.Data.Result, &result); err != nil {
		t.Fatalf("result unmarshal: %v", err)
	}
	if len(result) != 1 || len(result[0].Values) < 2 {
		t.Fatalf("result = %+v", result)
	}
	// Counter grows 1/s, so rate ≈ 1.
	if v, ok := result[0].Values[0][1].(string); !ok || !strings.HasPrefix(v, "1") {
		t.Fatalf("rate value = %v, want ~1", result[0].Values[0][1])
	}
}

func TestBadRequests(t *testing.T) {
	api, _ := newAPIOverStore(t, true)
	h := api.Routes()
	for _, path := range []string{
		"/api/v1/query",                                     // missing query
		"/api/v1/query?query=sum(",                          // parse error
		"/api/v1/query_range?query=up",                      // missing range params
		"/api/v1/query_range?query=up&start=2&end=1&step=1", // end < start
	} {
		code, env := get(t, h, path)
		if code != http.StatusBadRequest || env.Status != "error" {
			t.Errorf("%s: code=%d env=%+v, want 400 error", path, code, env)
		}
	}
}

func TestHealthz(t *testing.T) {
	ready, _ := newAPIOverStore(t, true)
	notReady, _ := newAPIOverStore(t, false)

	if code, _ := get(t, ready.Routes(), "/healthz/live"); code != http.StatusOK {
		t.Errorf("live = %d, want 200", code)
	}
	if code, _ := get(t, ready.Routes(), "/healthz/ready"); code != http.StatusOK {
		t.Errorf("ready(true) = %d, want 200", code)
	}
	if code, _ := get(t, notReady.Routes(), "/healthz/live"); code != http.StatusOK {
		t.Errorf("live(not ready) = %d, want 200", code)
	}
	if code, _ := get(t, notReady.Routes(), "/healthz/ready"); code != http.StatusServiceUnavailable {
		t.Errorf("ready(false) = %d, want 503", code)
	}
}
