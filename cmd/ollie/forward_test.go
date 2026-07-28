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
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
)

func TestAttrsToOTelAllowlist(t *testing.T) {
	kvs, dropped := attrsToOTel(map[string]string{
		"k8s.pod.name":              "nginx-abc",
		"k8s.namespace.name":        "tenant-a",
		"http.request.method":       "GET",
		"http.response.status_code": "200",
		"http.route":                "/**",
		"url.path":                  "/users/42?token=hunter2",
		"client.address":            "10.0.0.7",
		"user_agent.original":       "curl/8.0",
	})

	gotKeys := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		gotKeys = append(gotKeys, string(kv.Key))
	}
	sort.Strings(gotKeys)
	wantKeys := []string{
		"http.request.method", "http.response.status_code",
		"http.route", "k8s.namespace.name", "k8s.pod.name",
	}
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("forwarded keys = %v, want %v", gotKeys, wantKeys)
	}

	sort.Strings(dropped)
	wantDropped := []string{"client.address", "url.path", "user_agent.original"}
	if strings.Join(dropped, ",") != strings.Join(wantDropped, ",") {
		t.Errorf("dropped keys = %v, want %v", dropped, wantDropped)
	}
}

func TestAttrsToOTelEmpty(t *testing.T) {
	kvs, dropped := attrsToOTel(nil)
	if kvs != nil || dropped != nil {
		t.Errorf("attrsToOTel(nil) = (%v, %v), want (nil, nil)", kvs, dropped)
	}
}

// TestForwarderRecordFiltersLabels drives a MetricEvent through the
// real forwarder + OTel SDK and asserts the exported datapoint carries
// only allowlisted labels, with drops accounted in the self-obs
// counter.
func TestForwarderRecordFiltersLabels(t *testing.T) {
	ctx := context.Background()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	fwd := newMetricForwarder(mp.Meter("test"))

	fwd.Record(ctx, capture.MetricEvent{
		Name:  "http.server.request.duration",
		Value: 0.25,
		Attributes: map[string]string{
			"k8s.pod.name": "nginx-abc",
			"url.path":     "/users/42",
		},
	})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var sawForwarded, sawDropCounter bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "http.server.request.duration":
				sawForwarded = true
				sum, ok := m.Data.(metricdata.Sum[float64])
				if !ok {
					t.Fatalf("unexpected data type %T", m.Data)
				}
				for _, dp := range sum.DataPoints {
					if _, ok := dp.Attributes.Value("url.path"); ok {
						t.Error("url.path leaked into exported datapoint")
					}
					if _, ok := dp.Attributes.Value("k8s.pod.name"); !ok {
						t.Error("k8s.pod.name missing from exported datapoint")
					}
				}
			case "ollie_forward_labels_dropped_total":
				sawDropCounter = true
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("unexpected drop-counter type %T", m.Data)
				}
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				if total != 1 {
					t.Errorf("dropped-label count = %d, want 1", total)
				}
			}
		}
	}
	if !sawForwarded {
		t.Error("forwarded metric not exported")
	}
	if !sawDropCounter {
		t.Error("ollie_forward_labels_dropped_total not exported")
	}
}
