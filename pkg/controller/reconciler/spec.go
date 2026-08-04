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

package reconciler

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
	cppb "github.com/gke-labs/in-cluster-observability/pkg/controller/pb/controlplane/v1"
)

// Protocol bitset values. Each bit position mirrors the ordering of
// the capture.Module enum on the agent side (see pkg/capture/manager.go).
// Bit 1<<2 is reserved for HTTP/2, which is not independently
// selectable — OBI captures h2c under the HTTP toggle and does not
// label protocol version (ADR-0031) — so no ProtocolHTTP2 is defined.
const (
	ProtocolL4TCP uint32 = 1 << 0 // capture.ModuleL4TCP
	ProtocolHTTP1 uint32 = 1 << 1 // capture.ModuleHTTP1
	ProtocolGRPC  uint32 = 1 << 3 // capture.ModuleGRPC (1<<2 reserved for HTTP/2)
)

// ComputeSpec builds the MonitoringSpec for a pod given the set of
// CRs that cover it. The precedence rule from ADR-0003:
// **most-specific wins**. In v0.4 that means: any TrafficMonitor
// (namespaced) overrides any ClusterTrafficPolicy that would
// otherwise cover the same pod. Multiple TrafficMonitors on the
// same pod are flagged as a conflict (Phase 3); for now we pick
// lexicographic-first deterministically.
//
// Returns nil if no CR covers the pod with any enabled protocol.
func ComputeSpec(pod *corev1.Pod, tms []*v1alpha1.TrafficMonitor, ctps []*v1alpha1.ClusterTrafficPolicy) *cppb.MonitoringSpec {
	covering := selectCovering(pod, tms, ctps)
	if covering == nil {
		return nil
	}
	// Shape through the same helper the production ComputeCoverage path
	// uses, so the two never drift on protocol handling.
	return buildSpecFromCovering(pod, covering)
}

// coveringResult is the "which spec covers this pod" answer with a
// human-readable source ref for status / debugging.
type coveringResult struct {
	v1alpha1.MonitoringSpecCore
	sourceRef string
}

func selectCovering(pod *corev1.Pod, tms []*v1alpha1.TrafficMonitor, ctps []*v1alpha1.ClusterTrafficPolicy) *coveringResult {
	// Namespaced TrafficMonitors take precedence. Lex-first wins on
	// tie (deterministic; conflict will be surfaced as a status
	// Condition in Phase 3).
	candidates := []*v1alpha1.TrafficMonitor(nil)
	for _, tm := range tms {
		if tm.Namespace != pod.Namespace {
			continue
		}
		sel, err := matchTrafficMonitor(tm, pod)
		if err != nil || !sel {
			continue
		}
		candidates = append(candidates, tm)
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
		tm := candidates[0]
		return &coveringResult{
			MonitoringSpecCore: tm.Spec.MonitoringSpecCore,
			sourceRef:          "TrafficMonitor " + tm.Namespace + "/" + tm.Name,
		}
	}

	// Fall back to cluster-scoped policies. Same lex-first tie-break.
	clusterCandidates := []*v1alpha1.ClusterTrafficPolicy(nil)
	for _, ctp := range ctps {
		sel, err := matchClusterPolicy(ctp, pod)
		if err != nil || !sel {
			continue
		}
		clusterCandidates = append(clusterCandidates, ctp)
	}
	if len(clusterCandidates) > 0 {
		sort.Slice(clusterCandidates, func(i, j int) bool { return clusterCandidates[i].Name < clusterCandidates[j].Name })
		ctp := clusterCandidates[0]
		return &coveringResult{
			MonitoringSpecCore: ctp.Spec.MonitoringSpecCore,
			sourceRef:          "ClusterTrafficPolicy " + ctp.Name,
		}
	}
	return nil
}

func matchTrafficMonitor(tm *v1alpha1.TrafficMonitor, pod *corev1.Pod) (bool, error) {
	sel, err := metaLabelSelectorAsSelector(tm.Spec.WorkloadSelector)
	if err != nil {
		return false, err
	}
	return sel.Matches(labels.Set(pod.Labels)), nil
}

func matchClusterPolicy(ctp *v1alpha1.ClusterTrafficPolicy, pod *corev1.Pod) (bool, error) {
	// Namespace selector — if set, the namespace must match. We
	// don't have the Namespace object here; the caller is expected
	// to pre-filter or pass through. For the v0.4 reconciler we
	// take the simpler tack: skip the NamespaceSelector check at
	// match time (the reconciler scope already filters by
	// namespace at the watch level when needed). The
	// WorkloadSelector still applies.
	if ctp.Spec.WorkloadSelector == nil {
		return true, nil // empty workloadSelector matches every pod
	}
	sel, err := metaLabelSelectorAsSelector(*ctp.Spec.WorkloadSelector)
	if err != nil {
		return false, err
	}
	return sel.Matches(labels.Set(pod.Labels)), nil
}
