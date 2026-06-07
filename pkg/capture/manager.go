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

// Package capture is the only package in the project that may import
// OpenTelemetry eBPF Instrumentation (OBI,
// go.opentelemetry.io/obi/...). Every other package consumes capture
// via the Manager interface defined here. Per ADR-0010, this isolates
// OBI's v0 churn to one file.
//
// v0.1 ships the interface surface and a no-op Manager only — the
// real OBI integration lands in v0.2 (issues #70 / #71 / #72 / #73).
// Until then New(Config) returns a Manager whose Events() channel is
// already closed and whose lifecycle methods are no-ops.
//
// Stability: Experimental — every export in this package may break
// across MINOR versions until OBI hits 1.0.
package capture

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// Manager is the agent's handle on the eBPF capture pipeline. One
// per agent process. Method semantics:
//   - Start: load eBPF programs, attach probes, begin reading events.
//   - Stop: detach, close the Events() channel, release resources.
//   - AllowPID/BlockPID: idempotent per-PID enable/disable.
//     Used by the loopback debug endpoint and by tests; pass a
//     real process PID.
//   - AllowPod/BlockPod: idempotent per-pod enable/disable. Used
//     by the v0.4 controller-driven path — the agent writes K8s
//     metadata selectors that OBI matches against its own
//     informer-attached attributes, no PID resolution needed.
//   - EnableModule/DisableModule: idempotent per-protocol enable.
//   - Events: a buffered channel of translated capture events. Closed
//     on Stop. Readers must drain.
//   - AddEnricher: registers a hook that mutates events on the hot
//     path; called synchronously per event in registration order.
//   - Metrics: handle to the self-observability counters.
//
// Stability: Experimental
type Manager interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error

	AllowPID(pid uint32, spec PIDSpec) error
	BlockPID(pid uint32) error

	AllowPod(uid string, spec PodSpec) error
	BlockPod(uid string) error

	EnableModule(m Module, cfg ModuleConfig) error
	DisableModule(m Module) error
	EnabledModules() []Module

	Events() <-chan Event

	AddEnricher(Enricher)

	Metrics() *Metrics
}

// Config governs construction. Fields are additive; embedders rely
// on zero-value defaults wherever sensible.
//
// Stability: Experimental
type Config struct {
	// KubeletAddr is the URL the agent uses to query the node-local
	// Kubelet for the PID-to-Pod cache bootstrap (v0.3 / pkg/topology).
	// Defaults to "https://127.0.0.1:10250" when empty.
	KubeletAddr string
	// ProcPath is the mount point of host /proc inside the agent
	// container. Defaults to "/proc" when empty.
	ProcPath string
	// BpfFSPath is the bpffs mount point used for map pinning. Unused
	// in the sibling-container model (OBI owns the bpffs mount); kept
	// for forward compatibility.
	BpfFSPath string
	// EventBuffer sizes the Events() channel. Defaults to 4096 when
	// zero or negative.
	EventBuffer int

	// --- Sibling-container fields (per ADR-0018) ---

	// ObiConfigPath is the on-disk YAML file the agent writes for the
	// sibling OBI container to consume. Empty disables the config
	// writer (useful for tests). Default in production: shared volume
	// at /etc/ollie/obi-config/config.yaml.
	ObiConfigPath string
	// OTLPGRPCAddr is the loopback bind address for the OTLP gRPC
	// receiver. Empty disables the gRPC receiver. Must be loopback.
	OTLPGRPCAddr string
	// OTLPHTTPAddr is the loopback bind address for the OTLP HTTP
	// receiver. Empty disables the HTTP receiver. Must be loopback.
	OTLPHTTPAddr string
	// OBIEndpoint is the URL the agent tells OBI to push OTLP to
	// (written into ObiConfigPath). MUST include a scheme — OBI's
	// HTTP exporter parses the value as a net/url URL and rejects
	// bare host:port with "first path segment cannot contain colon".
	// Defaults to "http://<OTLPHTTPAddr>" when empty (OBI defaults to
	// the HTTP exporter, so we point at our HTTP listener).
	OBIEndpoint string
	// MeterProvider supplies the OTel meter used for self-observability
	// metrics. Defaults to capture.DefaultMeterProvider() when nil.
	MeterProvider metric.MeterProvider

	// InitialOpenPorts seeds OBI's discovery.instrument list with one
	// synthetic "smoke" entry matching any process whose listening
	// port is in this set. Format is OBI's native open_ports string:
	// a single port ("80"), comma list ("80,8080"), or range
	// ("8000-8999"). Only applied when no AllowPID-driven entries
	// exist, so it is harmless once the v0.4 controller starts
	// pushing per-PID MonitoringSpecs. Empty by default. Intended
	// for v0.3 smoke tests of L7 capture before the controller
	// exists — without it OBI's Application mode has nothing to
	// attach to and stays silent.
	InitialOpenPorts string
}

func (c *Config) applyDefaults() {
	if c.EventBuffer <= 0 {
		c.EventBuffer = 4096
	}
	if c.OBIEndpoint == "" && c.OTLPHTTPAddr != "" {
		c.OBIEndpoint = "http://" + c.OTLPHTTPAddr
	}
}

// PIDSpec is the per-PID enable record passed to AllowPID. Used by
// the loopback debug endpoint and by tests; the v0.4 controller path
// uses PodSpec instead so OBI's informer-attached attributes drive
// the match.
//
// Stability: Experimental
type PIDSpec struct {
	// Protocols is the set of capture modules to enable for this PID.
	Protocols []Module
	// Sampling is an optional per-PID sampling override; zero value
	// keeps the module-level default.
	Sampling Sampling
	// Labels are additional attributes the enricher attaches to events
	// produced for this PID.
	Labels map[string]string
}

// PodSpec is the per-pod enable record passed to AllowPod. The
// controller hands the agent one PodSpec per matched pod; the agent
// translates each into an obiconfig.Instrument entry whose K8s
// metadata selectors (k8s_pod_name + k8s_namespace) match the
// attributes OBI's K8s informer attaches per ADR-0021. This sidesteps
// PID resolution entirely — the agent never needs to know which host
// PID corresponds to which pod.
//
// Stability: Experimental
type PodSpec struct {
	// PodName is the pod's metadata.name. Required.
	PodName string
	// Namespace is the pod's metadata.namespace. Required.
	Namespace string
	// HTTPPorts is the set of listening ports OBI should attach L7
	// uprobes for (matches OBI's open_ports selector). Empty means
	// "no L7 ports configured"; combined with OBI's K8s-metadata
	// match, this still produces an instrument entry but it'll only
	// surface L4 flows (which OBI's socket filter captures
	// regardless of discovery).
	HTTPPorts []uint16
	// Protocols is the set of capture modules to enable for this pod.
	// Currently advisory — OBI's module gating is via
	// OTEL_EBPF_METRICS_FEATURES at sidecar startup; per-pod
	// per-module gating arrives with v0.6's richer Module surface.
	Protocols []Module
	// Labels are additional attributes the enricher attaches to
	// events produced for this pod. Inert in v0.4 (OBI's informer
	// already attaches K8s labels); reserved for v0.5+ when the
	// in-cluster store cares about per-CR-supplied tags.
	Labels map[string]string
}

// Sampling expresses head-based sampling rates. v0.1 is a struct stub;
// fields fill in alongside the sampling implementation (issue #109).
//
// Stability: Experimental
type Sampling struct {
	HeadRate float64 // 0..1; 0 means "use module default"
}

// Module enumerates the OBI tracer modules this project exposes. The
// numeric values are not part of the wire protocol; they are stable
// across minor versions but new modules may be added at the end.
//
// Stability: Experimental
type Module uint16

const (
	// ModuleL4TCP captures TCP-level counts and timings.
	ModuleL4TCP Module = iota + 1
	// ModuleHTTP1 captures plaintext HTTP/1.1.
	ModuleHTTP1
	// ModuleHTTP2 captures plaintext HTTP/2.
	ModuleHTTP2
	// ModuleGRPC captures gRPC on top of HTTP/2.
	ModuleGRPC
	// ModuleTLSGoCryptoTLS decrypts L7 over TLS for Go's crypto/tls.
	ModuleTLSGoCryptoTLS
	// ModuleTLSOpenSSL decrypts L7 over TLS for OpenSSL-using binaries.
	ModuleTLSOpenSSL
	// ModuleGenAI captures OpenAI / Anthropic / Gemini SDK calls.
	ModuleGenAI
)

// String returns a stable lowercase name for the Module.
//
// Stability: Experimental
func (m Module) String() string {
	switch m {
	case ModuleL4TCP:
		return "l4_tcp"
	case ModuleHTTP1:
		return "http1"
	case ModuleHTTP2:
		return "http2"
	case ModuleGRPC:
		return "grpc"
	case ModuleTLSGoCryptoTLS:
		return "tls_go_crypto_tls"
	case ModuleTLSOpenSSL:
		return "tls_openssl"
	case ModuleGenAI:
		return "genai"
	default:
		return "unknown"
	}
}

// ModuleConfig is the per-module tunable bag. v0.1 is empty; module
// implementations grow their own fields as they land.
//
// Stability: Experimental
type ModuleConfig struct{}

// EventKind discriminates the Event union.
//
// Stability: Experimental
type EventKind uint8

const (
	EventUnknown EventKind = iota
	EventMetric
	EventSpan
	EventEdge
	// EventModuleDegraded is emitted when a Module is forced into the
	// degraded state by panic recovery or kernel-verifier denial.
	EventModuleDegraded
)

// Event is the project-owned shape of a single record translated from
// OBI's internal types by the adapter. Exactly one of Metric, Span, or
// Edge is set per Event (matching Kind); for EventModuleDegraded all
// three are nil and Module names the affected module.
//
// Stability: Experimental
type Event struct {
	Kind      EventKind
	Timestamp time.Time
	PID       uint32
	Module    Module

	Metric *MetricEvent
	Span   *SpanEvent
	Edge   *EdgeEvent
}

// MetricEvent carries a single translated metric datapoint from OBI.
// Per ADR-0017.5, v0.2 carries the minimal field set; richer attributes
// (e.g. k8s.* resource attrs after enrichment) land in v0.3.
//
// Stability: Experimental
type MetricEvent struct {
	// Name is the metric name as emitted by OBI (e.g. tcp.rx.bytes).
	// Per ADR-0021 the translator passes names through unchanged; no
	// `ollie_*` prefix rewrite.
	Name string
	// Value is the datapoint's value at the report time (counters
	// arrive as deltas / sums per OBI's aggregation; this field carries
	// the raw value reported).
	Value float64
	// Attributes is the merged set of resource + datapoint attributes.
	// Per ADR-0021 OBI is the source of K8s identity, so k8s.* /
	// service.* attrs flow through unchanged for downstream re-emission.
	Attributes map[string]string
}

// SpanEvent carries a single translated OTel-shaped span from OBI's
// L7 capture (HTTP/1.1 in v0.2). Per ADR-0017.5, field set is
// minimal — full OTel span model (attribute soup + events + links)
// arrives in v0.3.
//
// Stability: Experimental
type SpanEvent struct {
	// Name is the span name as emitted by OBI (typically the OTel
	// semconv form, e.g. "GET /users/{id}" or "GET").
	Name string
	// Method is the HTTP method (GET, POST, ...).
	Method string
	// Path is the raw, untemplated URL path. Templating arrives in
	// v0.6 (#108).
	Path string
	// StatusCode is the HTTP response status (0 if unknown).
	StatusCode int
	// DurationNs is the span duration in nanoseconds.
	DurationNs uint64
	// Attributes is the merged set of resource + span attributes.
	// Per ADR-0021 OBI's k8s.* / service.* attrs flow through.
	Attributes map[string]string
}

// EdgeEvent carries a single topology edge record. Field set lands
// with the topology subsystem in v0.5.
//
// Stability: Experimental
type EdgeEvent struct{}

// Enricher is the hook signature; called synchronously per event in
// registration order before the writer dispatches.
//
// Stability: Experimental
type Enricher func(ctx context.Context, ev *Event)

// Metrics is the self-observability handle exposed by Manager. Counter
// names use the canonical ollie_capture_* prefix per
// docs/design/operations.md §5. Construct via NewMetrics; see metrics.go.
//
// Stability: Experimental
type Metrics struct {
	// Meter is the OTel meter scoped to the capture subsystem.
	// Embedders may create additional instruments off this if needed,
	// but should prefer the named fields below for the standard set.
	Meter metric.Meter

	// EventsTotal counts every translated capture.Event emitted to the
	// Events() channel. Attributes: module, kind.
	EventsTotal metric.Int64Counter

	// EventsDroppedTotal counts events that were not delivered.
	// Attributes: reason (backpressure | translation_error | shutdown).
	EventsDroppedTotal metric.Int64Counter

	// ActivePIDs is the current count of PIDs in the AllowPID set.
	// Up/down counter — adjusted on AllowPID / BlockPID.
	ActivePIDs metric.Int64UpDownCounter

	// ObiReloadsTotal counts OBI config-reload signals issued by the
	// agent. Attributes: result (success | failure).
	ObiReloadsTotal metric.Int64Counter

	// ObiRestartsTotal counts observed restarts of the sibling OBI
	// container, sourced from a container-status watcher (lands with
	// #77). Zero in v0.2 until that watcher ships.
	ObiRestartsTotal metric.Int64Counter

	// PanicsTotal counts recovered panics on the agent side.
	// Attributes: component (receiver | translator | writer | enricher).
	PanicsTotal metric.Int64Counter
}
