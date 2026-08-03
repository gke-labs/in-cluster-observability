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

package obi

import (
	"flag"
	"path/filepath"
	"testing"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// seedFixtures is gated by a -seed flag so it doesn't run as part of
// normal tests. Operators (or this commit, to bootstrap) invoke
// `go test -seed -update` to write synthetic fixtures and their
// golden outputs into testdata/translation/.
//
// Once real OBI fixtures are recorded (per REGENERATE.md), these
// synthetic ones should be replaced. The synthetic versions are
// intentionally minimal so the diff against real OBI is small.
var seedFixtures = flag.Bool("seed", false, "write synthetic fixtures into testdata/translation/")

func TestSeedFixtures(t *testing.T) {
	if !*seedFixtures {
		t.Skip("seed-only test; pass -seed to run")
	}

	// L4 basic: tcp.rx.bytes + tcp.tx.bytes for one workload.
	writeBinpb(t, filepath.Join("testdata", "translation", "l4-basic"), "metrics",
		&collmetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						strKV("service.namespace", "shop"),
						strKV("custom.tag", "kept"),
					},
				},
				ScopeMetrics: []*metricspb.ScopeMetrics{{
					Metrics: []*metricspb.Metric{
						sumIntMetric("tcp.rx.bytes", 4096, strKV("peer.address", "10.0.0.5")),
						sumIntMetric("tcp.tx.bytes", 2048, strKV("peer.address", "10.0.0.5")),
					},
				}},
			}},
		})

	// HTTP/1 basic: one GET span.
	writeBinpb(t, filepath.Join("testdata", "translation", "http1-basic"), "traces",
		&colltracepb.ExportTraceServiceRequest{
			ResourceSpans: []*tracepb.ResourceSpans{{
				ScopeSpans: []*tracepb.ScopeSpans{{
					Spans: []*tracepb.Span{{
						Name:              "GET /users/42",
						StartTimeUnixNano: 1_000_000_000,
						EndTimeUnixNano:   1_012_000_000,
						Attributes: []*commonpb.KeyValue{
							strKV("http.request.method", "GET"),
							strKV("url.path", "/users/42"),
							strKV("http.response.status_code", "200"),
						},
					}},
				}},
			}},
		})

	// gRPC basic: one unary-call span. OBI v0.10.0 attributes gRPC with
	// semconv v1.41.0 rpc.* keys (rpc.system.name="grpc", rpc.method =
	// the full path, rpc.response.status_code); the span name is the
	// full method path. This must translate to a ModuleGRPC span with
	// the RPC fields promoted and no HTTP fields (ADR-0031). Synthetic
	// until a real OBI recording replaces it (REGENERATE.md, #105).
	writeBinpb(t, filepath.Join("testdata", "translation", "grpc-basic"), "traces",
		&colltracepb.ExportTraceServiceRequest{
			ResourceSpans: []*tracepb.ResourceSpans{{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						strKV("service.namespace", "shop"),
						strKV("k8s.pod.name", "echo-0"),
					},
				},
				ScopeSpans: []*tracepb.ScopeSpans{{
					Spans: []*tracepb.Span{{
						Name:              "/grpc.health.v1.Health/Check",
						StartTimeUnixNano: 1_000_000_000,
						EndTimeUnixNano:   1_003_000_000,
						Attributes: []*commonpb.KeyValue{
							strKV("rpc.system.name", "grpc"),
							strKV("rpc.method", "/grpc.health.v1.Health/Check"),
							strKV("rpc.response.status_code", "0"),
						},
					}},
				}},
			}},
		})

	// gRPC RED metric: server call duration histogram-free stand-in
	// (a Sum is enough to exercise classifyMetric → ModuleGRPC). The
	// real OBI recording emits rpc.server.call.duration as a histogram.
	writeBinpb(t, filepath.Join("testdata", "translation", "grpc-metric-basic"), "metrics",
		&collmetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{strKV("service.namespace", "shop")},
				},
				ScopeMetrics: []*metricspb.ScopeMetrics{{
					Metrics: []*metricspb.Metric{
						sumIntMetric("rpc.server.call.duration", 3,
							strKV("rpc.method", "/grpc.health.v1.Health/Check"),
							strKV("rpc.response.status_code", "0")),
					},
				}},
			}},
		})
}

// strKV / sumIntMetric are local to the test package; they mirror the
// helpers in pkg/capture/translate_test.go.

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func sumIntMetric(name string, value int64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Sum{
			Sum: &metricspb.Sum{
				DataPoints: []*metricspb.NumberDataPoint{{
					Value:      &metricspb.NumberDataPoint_AsInt{AsInt: value},
					Attributes: attrs,
				}},
			},
		},
	}
}
