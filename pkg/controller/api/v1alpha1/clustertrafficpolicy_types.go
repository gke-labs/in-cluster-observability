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

// ClusterTrafficPolicySpec is the cluster-scoped default policy
// applied to every pod the operator chooses to cover. Per ADR-0003,
// a namespaced TrafficMonitor takes precedence over this default for
// pods it matches.
type ClusterTrafficPolicySpec struct {
	// NamespaceSelector restricts which namespaces this policy
	// applies to. Empty selector matches every namespace
	// (cluster-wide default). Same LabelSelector semantics as
	// TrafficMonitor.WorkloadSelector but scoped to namespace
	// labels, not pod labels.
	//
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// WorkloadSelector picks pods within the matched namespaces.
	// Empty selector matches every pod. The default-of-defaults is
	// (NamespaceSelector: empty, WorkloadSelector: empty) which
	// covers every pod in every namespace.
	//
	// +optional
	WorkloadSelector *metav1.LabelSelector `json:"workloadSelector,omitempty"`

	// MonitoringSpecCore — protocol toggles, ports, etc. See
	// shared_types.go. No validation marker here: this is an inline
	// embed, so a Required marker would add the empty json name ("") to
	// the parent's required list and make the apiserver reject every CR
	// with `spec.: Required value`. The embedded fields carry their own
	// markers (Protocols is Required in shared_types.go).
	MonitoringSpecCore `json:",inline"`
}

// ClusterTrafficPolicyStatus is the standard CR status surface.
type ClusterTrafficPolicyStatus struct {
	// MonitoringStatusCore — observedGeneration, matchedPodCount,
	// conditions, etc. See shared_types.go. matchedPodCount on a
	// ClusterTrafficPolicy reflects only the pods where this
	// default actually applies (after a more-specific
	// TrafficMonitor overrides it for that pod, this default no
	// longer counts).
	MonitoringStatusCore `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ctp,categories={ollie}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.matchedPodCount`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activelyMonitoredPodCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterTrafficPolicy declares cluster-wide default network capture
// intent, applied to any pod not already covered by a more-specific
// TrafficMonitor. Operator-owned. See docs/design/control-plane.md §1.2.
type ClusterTrafficPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterTrafficPolicySpec   `json:"spec,omitempty"`
	Status ClusterTrafficPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterTrafficPolicyList is the standard List wrapper.
type ClusterTrafficPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterTrafficPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterTrafficPolicy{}, &ClusterTrafficPolicyList{})
}
