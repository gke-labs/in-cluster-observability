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

package capture

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func sumMetric(name string, value int64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Sum{
			Sum: &metricspb.Sum{
				DataPoints: []*metricspb.NumberDataPoint{
					{
						Value:      &metricspb.NumberDataPoint_AsInt{AsInt: value},
						Attributes: attrs,
					},
				},
			},
		},
	}
}

func gaugeMetric(name string, value float64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Gauge{
			Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{
					{
						Value:      &metricspb.NumberDataPoint_AsDouble{AsDouble: value},
						Attributes: attrs,
					},
				},
			},
		},
	}
}

func TestTranslateMetrics_L4TCP(t *testing.T) {
	rm := []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strKV("k8s.pod.name", "passes-through"),
				strKV("service.name", "passes-through"),
				strKV("custom.attr", "kept"),
			},
		},
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Metrics: []*metricspb.Metric{
				sumMetric("tcp.rx.bytes", 4096,
					strKV("peer.address", "10.0.0.5"),
					intKV("peer.port", 8080),
				),
				sumMetric("tcp.tx.bytes", 2048,
					strKV("peer.address", "10.0.0.5"),
					intKV("peer.port", 8080),
				),
				gaugeMetric("tcp.rtt", 0.0012,
					strKV("peer.address", "10.0.0.5"),
				),
			},
		}},
	}}

	events := TranslateMetrics(rm)
	if len(events) != 3 {
		t.Fatalf("expected 3 events; got %d", len(events))
	}

	for _, ev := range events {
		if ev.Kind != EventMetric {
			t.Errorf("kind = %v; want metric", ev.Kind)
		}
		if ev.Module != ModuleL4TCP {
			t.Errorf("L4 metric should classify as ModuleL4TCP; got %v for %q", ev.Module, ev.Metric.Name)
		}
		if ev.Metric == nil {
			t.Error("Metric payload nil")
			continue
		}
		// Per ADR-0021: OBI is the source of K8s identity. k8s.* and
		// service.* attrs must pass through so re-emitted Prometheus
		// metrics carry them.
		if got := ev.Metric.Attributes["k8s.pod.name"]; got != "passes-through" {
			t.Errorf("k8s.pod.name should pass through (ADR-0021); got %q in %v", got, ev.Metric.Attributes)
		}
		if got := ev.Metric.Attributes["service.name"]; got != "passes-through" {
			t.Errorf("service.name should pass through (ADR-0021); got %q in %v", got, ev.Metric.Attributes)
		}
		if got := ev.Metric.Attributes["custom.attr"]; got != "kept" {
			t.Errorf("non-k8s resource attr should pass through; got custom.attr=%q", got)
		}
		if got := ev.Metric.Attributes["peer.address"]; got != "10.0.0.5" {
			t.Errorf("datapoint peer.address missing or wrong; got %q", got)
		}
	}
}

func TestTranslateMetrics_HTTPClassification(t *testing.T) {
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Metrics: []*metricspb.Metric{
				sumMetric("http.server.requests", 1),
				gaugeMetric("http_request_duration", 0.05),
			},
		}},
	}}
	events := TranslateMetrics(rm)
	if len(events) != 2 {
		t.Fatalf("expected 2 events; got %d", len(events))
	}
	for _, ev := range events {
		if ev.Module != ModuleHTTP1 {
			t.Errorf("HTTP-shaped metric name %q should classify as ModuleHTTP1; got %v", ev.Metric.Name, ev.Module)
		}
	}
}

func TestTranslateMetrics_GRPCClassification(t *testing.T) {
	// OBI's gRPC RED metrics use the rpc.* family (OTEL dotted and
	// Prometheus underscore forms). Both must classify as ModuleGRPC,
	// distinct from HTTP (ADR-0031).
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Metrics: []*metricspb.Metric{
				sumMetric("rpc.server.call.duration", 1),
				gaugeMetric("rpc_client_call_duration_seconds", 0.05),
			},
		}},
	}}
	events := TranslateMetrics(rm)
	if len(events) != 2 {
		t.Fatalf("expected 2 events; got %d", len(events))
	}
	for _, ev := range events {
		if ev.Module != ModuleGRPC {
			t.Errorf("rpc-shaped metric name %q should classify as ModuleGRPC; got %v", ev.Metric.Name, ev.Module)
		}
	}
}

func TestTranslateMetrics_EmptyInput(t *testing.T) {
	if got := TranslateMetrics(nil); len(got) != 0 {
		t.Errorf("nil input should produce no events; got %d", len(got))
	}
}
