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

// Package schema exports the canonical metric-name and attribute-key
// constants used throughout the project. Every other package
// references these constants rather than hard-coding strings so that a
// schema audit (and any future schema migration) has one place to look.
// See docs/design/storage-and-query.md §6 for the full schema.
package schema

// MetricPrefix is the namespace prefix every metric name carries.
//
// Stability: Experimental
const MetricPrefix = "ollie"

// Source-side label keys following OTel K8s semantic conventions.
//
// Stability: Experimental
const (
	LabelK8sPodName         = "k8s.pod.name"
	LabelK8sPodUID          = "k8s.pod.uid"
	LabelK8sNamespaceName   = "k8s.namespace.name"
	LabelK8sNodeName        = "k8s.node.name"
	LabelK8sContainerName   = "k8s.container.name"
	LabelK8sDeploymentName  = "k8s.deployment.name"
	LabelK8sStatefulSetName = "k8s.statefulset.name"
	LabelK8sDaemonSetName   = "k8s.daemonset.name"
	LabelK8sJobName         = "k8s.job.name"
	LabelK8sReplicaSetName  = "k8s.replicaset.name"
	LabelServiceName        = "service.name"
	LabelServiceInstanceID  = "service.instance.id"
	LabelServiceNamespace   = "service.namespace"
)

// Forwarded-label allowlist (#144). The agent's metric forwarder
// re-emits OBI-captured attributes as Prometheus labels on the :9090
// scrape endpoint; only keys passing ForwardableLabel survive. The
// endpoint's contents are bounded here, by construction, rather than
// by whatever attributes the pinned OBI version happens to emit —
// high-sensitivity keys (url.path, url.full, client.address,
// server.address, user_agent.original) are excluded deliberately.
//
// Stability: Experimental
var (
	// ForwardAllowedLabelPrefixes admits attribute families wholesale:
	// K8s identity (including k8s.src.* / k8s.dst.* dual-sided
	// attribution on L4 flows), service identity, and protocol-level
	// network/tcp dimensions.
	ForwardAllowedLabelPrefixes = []string{"k8s.", "service.", "network.", "tcp."}

	// ForwardAllowedLabelKeys admits individual low-cardinality,
	// low-sensitivity dimensions that no prefix family covers.
	// http.route is safe because the agent pins OBI's routes.unmatched
	// to wildcard (see internal/obiconfig), which collapses unmatched
	// paths to "/**". direction (request|response|unknown) is the L4
	// flow half-of-connection on obi.network.flow.bytes; without it the
	// two directions carry identical labels and collapse to a single
	// series, last-write-wins over a monotonic counter (#170 review).
	ForwardAllowedLabelKeys = []string{
		"http.request.method",
		"http.response.status_code",
		"http.route",
		"direction",
	}
)

// ForwardableLabel reports whether the metric forwarder may re-emit
// the given OTLP attribute key as a Prometheus label.
//
// Stability: Experimental
func ForwardableLabel(key string) bool {
	for _, k := range ForwardAllowedLabelKeys {
		if key == k {
			return true
		}
	}
	for _, p := range ForwardAllowedLabelPrefixes {
		if len(key) > len(p) && key[:len(p)] == p {
			return true
		}
	}
	return false
}

// Peer-side label keys mirror the source-side namespace.
//
// Stability: Experimental
const (
	LabelPeerK8sPodName        = "peer.k8s.pod.name"
	LabelPeerK8sNamespaceName  = "peer.k8s.namespace.name"
	LabelPeerK8sDeploymentName = "peer.k8s.deployment.name"
	LabelPeerServiceName       = "peer.service.name"
	LabelPeerKind              = "peer.kind"
	LabelPeerExternal          = "peer.external"
	LabelPeerIP                = "peer.ip"
	LabelPeerPort              = "peer.port"
)
