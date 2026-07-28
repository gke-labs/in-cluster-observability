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

// Package obiconfig models the subset of OpenTelemetry eBPF
// Instrumentation (OBI) configuration that the agent controls via the
// shared config volume. Per ADR-0018, OBI runs as a sibling container
// and watches this file; the agent writes it on MonitoringSpec changes
// and OBI reloads.
//
// The schema mirrors OBI's YAML config keys for the fields we set
// (verified against otel/ebpf-instrument v0.9). It is intentionally a
// subset — operators retain control over OBI's other knobs via the
// install-time base config that this file overlays (TBD in v0.4 when
// the controller takes over).
package obiconfig

// File is the on-disk OBI config the agent writes. Marshals to YAML
// via gopkg.in/yaml.v3. Field names follow OBI's config schema.
type File struct {
	OtelMetricsExport OTLPExport `yaml:"otel_metrics_export"`
	OtelTracesExport  OTLPExport `yaml:"otel_traces_export"`
	Attributes        Attributes `yaml:"attributes"`
	Routes            *Routes    `yaml:"routes,omitempty"`
	Discovery         Discovery  `yaml:"discovery"`
}

// OTLPExport tells OBI where to send the corresponding telemetry. For
// the sibling-container model, both endpoints point at our agent's
// loopback OTLP receiver.
type OTLPExport struct {
	Endpoint string `yaml:"endpoint"`
}

// Attributes controls OBI's attribute attachment behavior.
type Attributes struct {
	Kubernetes KubernetesAttrs `yaml:"kubernetes"`
}

// KubernetesAttrs governs OBI's built-in K8s metadata attachment. Per
// ADR-0021, OBI is the single source of K8s identity on captured
// events; the agent attaches none of its own.
type KubernetesAttrs struct {
	Enable bool `yaml:"enable"`
}

// Routes governs OBI's URL-path handling. We leave templating to v0.6
// (#108) and set unmatched: wildcard, which collapses every unmatched
// path to a literal "/**" http.route — it does NOT preserve raw paths
// (that's the `path` option, which copies request paths into metric
// labels: a cardinality explosion and a data-sensitivity leak, #144).
type Routes struct {
	Unmatched string `yaml:"unmatched,omitempty"`
}

// Discovery is the per-target instrumentation selector list. The YAML
// key is `instrument` (OBI v0.9 glob-style); the legacy `services`
// key is the regex-style equivalent and the *output* schema for
// already-discovered services — a different shape.
type Discovery struct {
	Instrument []Instrument `yaml:"instrument,omitempty"`
}

// Instrument is one OBI discovery selector — a process matcher. OBI
// resolves all configured selectors and instruments any process that
// matches at least one. An entry must specify at least one selector
// (OpenPorts, TargetPIDs, ExePath, or similar) or OBI rejects the
// config at startup.
type Instrument struct {
	Name      string `yaml:"name,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
	// OpenPorts matches processes by listening port. OBI's IntEnum
	// accepts a scalar string ("80", "80,8080", "8000-8999") or a
	// YAML sequence of ints; we emit the string form for compactness.
	OpenPorts string `yaml:"open_ports,omitempty"`
	// TargetPIDs is OBI's `target_pids` selector — an explicit list
	// of process IDs to instrument. Populated by AllowPID-driven
	// entries (each entry typically targets a single PID).
	TargetPIDs []uint32 `yaml:"target_pids,omitempty"`
	// ExePath is a glob over the process executable path.
	ExePath string `yaml:"exe_path,omitempty"`
}

// DefaultFile returns a baseline File with the loopback OTLP endpoints
// configured and OBI's K8s metadata informer enabled (per ADR-0021 —
// OBI owns enrichment, the agent does none).
//
// Stability: Experimental
func DefaultFile(otlpEndpoint string) File {
	return File{
		OtelMetricsExport: OTLPExport{Endpoint: otlpEndpoint},
		OtelTracesExport:  OTLPExport{Endpoint: otlpEndpoint},
		Attributes:        Attributes{Kubernetes: KubernetesAttrs{Enable: true}},
		Routes:            &Routes{Unmatched: "wildcard"},
		Discovery:         Discovery{Instrument: nil},
	}
}
