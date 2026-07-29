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

// Package custommetrics serves the custom.metrics.k8s.io/v1beta1
// aggregated API (#96, ADR-0025 §6): the read path the Kubernetes
// HPA uses. It is deliberately minimal — v1beta1 discovery plus the
// namespaced-object metric GET the HPA issues — rather than a
// kubernetes-sigs/custom-metrics-apiserver embed; metric names map
// to PromQL templates loaded from an operator-editable config
// (ConfigMap-mounted file), evaluated against the fan-out queryable.
//
// v0.5 scope (recorded on #96): object metrics on concrete names
// only. Pods-type metrics with label-selector enumeration and
// aggregation hints ride with #111.
package custommetrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/prometheus/prometheus/promql"
)

// GroupVersion is the served API group-version.
const GroupVersion = "custom.metrics.k8s.io/v1beta1"

// basePath is where the aggregator proxies the group-version.
const basePath = "/apis/custom.metrics.k8s.io/v1beta1"

// Evaluator runs an instant PromQL query; implemented by
// queryapi.API over the fan-out.
type Evaluator interface {
	InstantVector(ctx context.Context, expr string, ts time.Time) (promql.Vector, error)
}

// FileConfig is the operator-editable YAML shape (ConfigMap-mounted).
// Both maps overlay the defaults; set a key to "" to remove it.
type FileConfig struct {
	// Metrics maps metric name → PromQL text/template. The template
	// receives {{.Selector}}, pre-rendered as
	// `k8s_namespace_name="<ns>",<groupLabel>="<name>"`.
	Metrics map[string]string `json:"metrics"`
	// Resources maps a resource plural (no group suffix) → the label
	// that identifies one object of that kind in stored series.
	// Empty string means "namespace selector only" (used by the
	// `metrics` pseudo-resource for namespace-level metrics).
	Resources map[string]string `json:"resources"`
}

// defaultMetrics per storage-and-query.md §7, adapted to the actual
// OBI-derived series names (ADR-0021 passthrough: OTel dots →
// underscores, no unit or _total suffixing). conn_rate from the
// original table is omitted — the pinned OBI image emits no
// connection-rate counter.
var defaultMetrics = map[string]string{
	"qps":               `sum(rate(http_server_request_duration_count{ {{.Selector}} }[1m]))`,
	"latency_p50":       `histogram_quantile(0.5, sum by (le) (rate(http_server_request_duration_bucket{ {{.Selector}} }[1m])))`,
	"latency_p99":       `histogram_quantile(0.99, sum by (le) (rate(http_server_request_duration_bucket{ {{.Selector}} }[1m])))`,
	"bytes_in_per_sec":  `sum(rate(http_server_request_body_size_sum{ {{.Selector}} }[1m]))`,
	"bytes_out_per_sec": `sum(rate(http_server_response_body_size_sum{ {{.Selector}} }[1m]))`,
}

// defaultResources: plural → grouping label. `metrics` is the API's
// namespace-metric pseudo-resource (GET .../namespaces/{ns}/metrics/{m}).
var defaultResources = map[string]string{
	"pods":         "k8s_pod_name",
	"deployments":  "k8s_deployment_name",
	"statefulsets": "k8s_statefulset_name",
	"daemonsets":   "k8s_daemonset_name",
	"services":     "service_name",
	"metrics":      "",
}

// describedKinds maps resource plural → (apiVersion, kind) for the
// describedObject echoed back in MetricValue.
var describedKinds = map[string]struct{ apiVersion, kind string }{
	"pods":         {"v1", "Pod"},
	"deployments":  {"apps/v1", "Deployment"},
	"statefulsets": {"apps/v1", "StatefulSet"},
	"daemonsets":   {"apps/v1", "DaemonSet"},
	"services":     {"v1", "Service"},
	"metrics":      {"v1", "Namespace"},
}

// Config for New.
type Config struct {
	Evaluator Evaluator
	// ConfigPath, when non-empty and existing, overlays the default
	// metric templates and resource labels. Loaded once at startup;
	// edit + rollout to change (v0.5 posture).
	ConfigPath string
	Logger     *slog.Logger
}

// Handler serves the aggregated API.
type Handler struct {
	eval      Evaluator
	metrics   map[string]*template.Template
	resources map[string]string
	logger    *slog.Logger
}

// New builds the handler, overlaying cfg.ConfigPath onto the
// defaults when present.
func New(cfg Config) (*Handler, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	metrics := map[string]string{}
	for k, v := range defaultMetrics {
		metrics[k] = v
	}
	resources := map[string]string{}
	for k, v := range defaultResources {
		resources[k] = v
	}
	if cfg.ConfigPath != "" {
		raw, err := os.ReadFile(cfg.ConfigPath)
		switch {
		case err == nil:
			var fc FileConfig
			if err := yaml.Unmarshal(raw, &fc); err != nil {
				return nil, fmt.Errorf("custommetrics: parse %s: %w", cfg.ConfigPath, err)
			}
			for k, v := range fc.Metrics {
				if v == "" {
					delete(metrics, k)
					continue
				}
				metrics[k] = v
			}
			for k, v := range fc.Resources {
				if v == "" && k != "metrics" {
					delete(resources, k)
					continue
				}
				resources[k] = v
			}
			cfg.Logger.Info("custom-metrics config loaded", "path", cfg.ConfigPath, "metrics", len(metrics))
		case os.IsNotExist(err):
			cfg.Logger.Info("custom-metrics config absent; using defaults", "path", cfg.ConfigPath)
		default:
			return nil, fmt.Errorf("custommetrics: read %s: %w", cfg.ConfigPath, err)
		}
	}

	h := &Handler{
		eval:      cfg.Evaluator,
		metrics:   map[string]*template.Template{},
		resources: resources,
		logger:    cfg.Logger,
	}
	for name, tmpl := range metrics {
		t, err := template.New(name).Parse(tmpl)
		if err != nil {
			return nil, fmt.Errorf("custommetrics: template %q: %w", name, err)
		}
		h.metrics[name] = t
	}
	return h, nil
}

// Routes mounts the API on a fresh mux.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+basePath, h.handleDiscovery)
	mux.HandleFunc("GET "+basePath+"/{$}", h.handleDiscovery)
	mux.HandleFunc("GET "+basePath+"/namespaces/{namespace}/{resource}/{name}/{metric}", h.handleMetric)
	return mux
}

// handleDiscovery serves the APIResourceList the HPA's client (and
// the aggregator's availability probe) reads.
func (h *Handler) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	var names []string
	for m := range h.metrics {
		names = append(names, m)
	}
	sort.Strings(names)
	var plurals []string
	for r := range h.resources {
		plurals = append(plurals, r)
	}
	sort.Strings(plurals)

	list := metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
		GroupVersion: GroupVersion,
	}
	for _, r := range plurals {
		for _, m := range names {
			list.APIResources = append(list.APIResources, metav1.APIResource{
				Name:       r + "/" + m,
				Namespaced: true,
				Kind:       "MetricValueList",
				Verbs:      metav1.Verbs{"get"},
			})
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// MetricValue / MetricValueList mirror the wire shape of
// custom.metrics.k8s.io/v1beta1 (hand-rolled per ADR-0025 §6 to keep
// the k8s.io/metrics module out of the dependency tree).
type MetricValue struct {
	DescribedObject corev1.ObjectReference `json:"describedObject"`
	MetricName      string                 `json:"metricName"`
	Timestamp       metav1.Time            `json:"timestamp"`
	Value           resource.Quantity      `json:"value"`
}

// MetricValueList is the GET response.
type MetricValueList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MetricValue `json:"items"`
}

func (h *Handler) handleMetric(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	resourceArg := r.PathValue("resource")
	name := r.PathValue("name")
	metricName := r.PathValue("metric")

	// The HPA addresses grouped resources as "<plural>.<group>"
	// (deployments.apps); kubectl examples use the bare plural.
	plural := resourceArg
	if i := strings.IndexByte(plural, '.'); i > 0 {
		plural = plural[:i]
	}

	label, ok := h.resources[plural]
	if !ok {
		writeStatusError(w, http.StatusNotFound, fmt.Sprintf("resource %q is not served", resourceArg))
		return
	}
	tmpl, ok := h.metrics[metricName]
	if !ok {
		writeStatusError(w, http.StatusNotFound, fmt.Sprintf("metric %q is not configured", metricName))
		return
	}
	if name == "*" {
		// Pods-type HPA metrics enumerate via selector; deferred to
		// #111 with aggregation hints.
		writeStatusError(w, http.StatusNotImplemented, "wildcard object names are not supported in v0.5; use an Object metric with a concrete name")
		return
	}

	selector := fmt.Sprintf("k8s_namespace_name=%q", ns)
	if label != "" {
		selector += fmt.Sprintf(",%s=%q", label, name)
	}
	var expr bytes.Buffer
	if err := tmpl.Execute(&expr, struct{ Selector string }{Selector: selector}); err != nil {
		writeStatusError(w, http.StatusInternalServerError, fmt.Sprintf("rendering template for %q: %v", metricName, err))
		return
	}

	now := time.Now()
	vec, err := h.eval.InstantVector(r.Context(), expr.String(), now)
	if err != nil {
		writeStatusError(w, http.StatusInternalServerError, fmt.Sprintf("evaluating %q: %v", metricName, err))
		return
	}
	if len(vec) == 0 {
		writeStatusError(w, http.StatusNotFound, fmt.Sprintf("no value for metric %q on %s/%s/%s (no matching series in the window)", metricName, ns, resourceArg, name))
		return
	}
	// A finite value is required. histogram_quantile over idle traffic
	// yields NaN, rate() over a single sample yields no points, and an
	// arithmetic template can overflow to ±Inf; NewMilliQuantity would
	// turn any of these into a garbage int64 the HPA scales on. Treat a
	// non-finite result the same as no value (404), so the HPA holds
	// its last-known replica count instead of chasing a bogus number.
	val := vec[0].F
	if math.IsNaN(val) || math.IsInf(val, 0) {
		writeStatusError(w, http.StatusNotFound, fmt.Sprintf("metric %q on %s/%s/%s evaluated to a non-finite value (%v); no usable data in the window", metricName, ns, resourceArg, name, val))
		return
	}

	dk := describedKinds[plural]
	obj := corev1.ObjectReference{
		Kind:       dk.kind,
		APIVersion: dk.apiVersion,
		Namespace:  ns,
		Name:       name,
	}
	if plural == "metrics" {
		obj.Name = ns
		obj.Namespace = ""
	}

	writeJSON(w, http.StatusOK, MetricValueList{
		TypeMeta: metav1.TypeMeta{Kind: "MetricValueList", APIVersion: GroupVersion},
		Items: []MetricValue{{
			DescribedObject: obj,
			MetricName:      metricName,
			Timestamp:       metav1.NewTime(now),
			Value:           *resource.NewMilliQuantity(int64(val*1000), resource.DecimalSI),
		}},
	})
}

func writeStatusError(w http.ResponseWriter, code int, msg string) {
	reason := metav1.StatusReasonInternalError
	switch code {
	case http.StatusNotFound:
		reason = metav1.StatusReasonNotFound
	case http.StatusNotImplemented:
		reason = metav1.StatusReasonMethodNotAllowed
	}
	writeJSON(w, code, metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Code:     int32(code),
		Reason:   reason,
		Message:  msg,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
