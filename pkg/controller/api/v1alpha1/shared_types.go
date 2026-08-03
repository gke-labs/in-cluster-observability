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

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MonitoringSpecCore is the common spec shared between TrafficMonitor
// and ClusterTrafficPolicy. v0.4 lands a minimal field set sufficient
// for the reconciler → MonitoringSpec → obiconfig.Instrument mapping
// documented in docs/design/control-plane.md §2.3. Richer cardinality
// controls (path templating, sampling, aggregation hints) arrive with
// the v0.6 hardening milestone (#108, #109, #110).
type MonitoringSpecCore struct {
	// Protocols toggles per-protocol capture for the matched pods.
	// Enabling at least one protocol is not schema-enforced: a CR with
	// no protocol enabled is admitted and the validating webhook emits a
	// warning (not a rejection) per ADR-0030 §3 — a no-op spec is valid
	// and simply monitors nothing.
	//
	// +kubebuilder:validation:Required
	Protocols ProtocolSet `json:"protocols"`
}

// ProtocolSet is the per-protocol capture toggles. Each block carries
// an `enabled` flag plus the ports OBI should attach to for that
// protocol.
type ProtocolSet struct {
	// L4 enables L4 TCP capture (bytes, RTT) via OBI's network mode.
	// L4 capture uses an in-kernel socket filter and ignores Ports —
	// it observes every TCP flow on the node regardless of port.
	//
	// +optional
	L4 *L4Config `json:"l4,omitempty"`

	// HTTP enables HTTP/1.1 capture via OBI's Application mode
	// (uprobe attach to processes listening on the configured ports).
	// HTTP/2 and gRPC support arrive in v0.6 (#104, #105).
	//
	// +optional
	HTTP *HTTPConfig `json:"http,omitempty"`
}

// L4Config configures L4 TCP capture.
type L4Config struct {
	// Enabled gates the entire L4 path. Default false.
	//
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`
}

// HTTPConfig configures HTTP/1.1 capture.
type HTTPConfig struct {
	// Enabled gates the entire HTTP path. Default false.
	//
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// Ports is the list of TCP ports to instrument. If a matched
	// pod's process opens one of these ports, OBI attaches L7
	// uprobes. Empty defaults to common HTTP ports (80, 8080) at
	// the controller. Each port must be in [1, 65535] — enforced by
	// the CRD schema so an out-of-range port is rejected even during
	// the webhook's failurePolicy=Ignore bootstrap window (the webhook
	// re-checks the same bound for a clear message).
	//
	// +kubebuilder:validation:MinItems=0
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	// +optional
	Ports []int32 `json:"ports,omitempty"`
}

// MonitoringStatusCore is the common status shared between the two
// CRDs. Both CRDs surface (a) reconciliation health and (b) how many
// pods their selector currently covers. ClusterTrafficPolicy adds no
// extra status fields in v0.4.
type MonitoringStatusCore struct {
	// ObservedGeneration is the .metadata.generation the reconciler
	// has fully applied. Used by clients to detect "spec changed but
	// status not yet updated."
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// MatchedPodCount is the number of pods currently matched by
	// this CR's workloadSelector. For ClusterTrafficPolicy this is
	// the count of pods this default applies to (after considering
	// more-specific TrafficMonitors that override).
	//
	// +optional
	MatchedPodCount int32 `json:"matchedPodCount,omitempty"`

	// ActivelyMonitoredPodCount is the number of matched pods that
	// have an active spec confirmed by their agent. May lag
	// MatchedPodCount briefly during reconciliation or agent rollout.
	//
	// +optional
	ActivelyMonitoredPodCount int32 `json:"activelyMonitoredPodCount,omitempty"`

	// MatchedPodSample is up to 5 pod names from the matched set
	// (sorted lexicographically for determinism). Helpful for
	// eyeballing "did my selector pick what I expected."
	//
	// +kubebuilder:validation:MaxItems=5
	// +optional
	MatchedPodSample []string `json:"matchedPodSample,omitempty"`

	// Conflicts lists other CRs whose selectors overlap with this
	// one with conflicting protocol settings. Each entry is the
	// fully-qualified CR name (`namespace/name` for TrafficMonitor;
	// just `name` for ClusterTrafficPolicy). Populated by the
	// reactive conflict-detection path that takes the deferred
	// validating webhook's place (ADR-0022.4).
	//
	// +optional
	Conflicts []string `json:"conflicts,omitempty"`

	// Conditions follow the standard K8s convention. Known types:
	// `Ready`, `Degraded`, `Conflict`.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Standard condition Type values used in MonitoringStatusCore.Conditions.
const (
	// ConditionReady is True when the CR's spec is fully applied to
	// all matched pods on all reachable agents. False during normal
	// reconciliation, after an error, or while conflicts are unresolved.
	ConditionReady = "Ready"

	// ConditionDegraded is True when the reconciler is making forward
	// progress but at least one matched pod is not currently being
	// monitored (agent disconnected, OBI degraded, etc.). The CR is
	// not Ready but is also not in a configuration-error state.
	ConditionDegraded = "Degraded"

	// ConditionConflict is True when this CR overlaps with another
	// CR whose spec disagrees on protocol settings for the same
	// pods. See MonitoringStatusCore.Conflicts for the offending
	// CR names. Lands in place of the deferred validating webhook.
	ConditionConflict = "Conflict"
)
