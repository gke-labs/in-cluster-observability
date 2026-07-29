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
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// NewPromMeterProvider constructs a MeterProvider whose readings are
// exposed in Prometheus text format via the returned http.Handler.
// The handler is suitable for mounting at e.g. /debug/metrics on the
// debug endpoint or on a separate listener.
//
// The MeterProvider should be passed into Config.MeterProvider so that
// pkg/capture's self-observability counters end up scrape-visible.
// The returned Registry is the one the handler serves — callers may
// register additional prometheus.Collectors on it (the agent's OBI
// re-emission collector does, #153).
//
// This is the v0.2 verification path; v0.3's Prometheus scrape sink
// (#82) will subsume it with the full per-component metric surface.
//
// Stability: Experimental
func NewPromMeterProvider() (*sdkmetric.MeterProvider, *prometheus.Registry, http.Handler, error) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		// Cumulative semantics match Prometheus's expectation; histograms
		// stay classic for v0.2 (native histograms are a v0.6 toggle per
		// docs/design/roadmap.md §7).
		otelprom.WithoutCounterSuffixes(),
		otelprom.WithoutUnits(),
		// Disable the SDK's auto-generated target_info / scope_info.
		// OBI emits its own target_info per discovered workload (with
		// the workload's K8s/cloud Resource attrs and empty help). The
		// forwarder filters OBI's copies, but we also drop ours so
		// there is only ever one possible source of target_info in
		// the registry — eliminates the help-text collision class
		// of bug entirely. The agent's identity is still scrapable
		// via ollie_agent_up.
		otelprom.WithoutTargetInfo(),
		otelprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("capture: prometheus exporter: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{}},
		)),
	)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry:          reg,
		EnableOpenMetrics: true,
	})
	return provider, reg, handler, nil
}

// Compile-time check that metricdata is referenced (some IDEs prune
// otherwise; we rely on the SDK transitively).
var _ = metricdata.Temporality(0)
