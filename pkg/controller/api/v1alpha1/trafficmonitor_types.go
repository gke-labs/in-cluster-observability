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

// TrafficMonitorSpec is the namespaced opt-in spec.
type TrafficMonitorSpec struct {
	// WorkloadSelector picks pods in this CR's namespace. Uses
	// standard K8s LabelSelector semantics (matchLabels and
	// matchExpressions). An empty selector matches every pod in
	// the namespace.
	//
	// +kubebuilder:validation:Required
	WorkloadSelector metav1.LabelSelector `json:"workloadSelector"`

	// MonitoringSpecCore — protocol toggles, ports, etc. See
	// shared_types.go. No validation marker here: this is an inline
	// embed, so a Required marker would add the empty json name ("") to
	// the parent's required list and make the apiserver reject every CR
	// with `spec.: Required value`. The embedded fields carry their own
	// markers (Protocols is Required in shared_types.go).
	MonitoringSpecCore `json:",inline"`
}

// TrafficMonitorStatus is the standard CR status surface.
type TrafficMonitorStatus struct {
	// MonitoringStatusCore — observedGeneration, matchedPodCount,
	// conditions, etc. See shared_types.go.
	MonitoringStatusCore `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=tm,categories={ollie}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.matchedPodCount`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activelyMonitoredPodCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TrafficMonitor declares per-workload network capture intent in a
// namespace. Workload teams own these CRs; selects the pods (via
// LabelSelector) and configures which protocols to capture. See
// docs/design/control-plane.md §1.1.
type TrafficMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrafficMonitorSpec   `json:"spec,omitempty"`
	Status TrafficMonitorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TrafficMonitorList is the standard List wrapper.
type TrafficMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrafficMonitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrafficMonitor{}, &TrafficMonitorList{})
}
