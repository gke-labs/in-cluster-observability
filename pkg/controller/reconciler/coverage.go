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
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
	cppb "github.com/gke-labs/in-cluster-observability/pkg/controller/pb/controlplane/v1"
)

// CoverageResult is what ComputeCoverage returns: the per-pod specs
// to dispatch + the per-CR rollups the reconciler writes back into
// CR status (Phase 3). One pass over inputs; both consumers branch
// off the same answers.
type CoverageResult struct {
	// Specs is the per-pod-UID MonitoringSpec the dispatcher hands
	// to agents. Pods with no covering CR or no enabled protocol
	// are absent (dispatcher emits REMOVE for vanished entries).
	Specs map[string]*cppb.MonitoringSpec

	// TMStatus is the per-TrafficMonitor status rollup keyed by
	// (namespace, name). For each TM: which pods it matched (the
	// raw selector match, before precedence), which it actually
	// "won" coverage for (after precedence + tie-break), and which
	// other TMs overlap with it on the matched set.
	TMStatus map[types.NamespacedName]*CRStatus

	// CTPStatus is the cluster-scoped equivalent. Keyed by name.
	CTPStatus map[string]*CRStatus
}

// CRStatus is the per-CR rollup the reconciler writes back.
type CRStatus struct {
	// MatchedPods is the set of pods the CR's selector matches
	// (regardless of whether another CR overrides coverage).
	MatchedPods []*corev1.Pod

	// CoveredPods is the subset of MatchedPods where this CR
	// "won" coverage (i.e. produced the MonitoringSpec). Excludes
	// pods where a more-specific CR overrode (TM-over-CTP) or a
	// lex-first TM took the tie.
	CoveredPods []*corev1.Pod

	// Conflicts lists other CR names (NamespacedName format for
	// TMs, just name for CTPs) whose selectors overlap with this
	// one's MatchedPods AND whose MonitoringSpecCore disagrees on
	// protocol settings. Reactive conflict detection per
	// ADR-0022.4 (takes the deferred webhook's place).
	Conflicts []string
}

// ComputeCoverage is the Phase-3 evolution of selectCovering: one
// pass over (pods × CRs) that produces both the dispatcher input
// (Specs) and the status writeback inputs (TMStatus / CTPStatus).
// Pure function; safe to call from a test.
func ComputeCoverage(pods []*corev1.Pod, tms []*v1alpha1.TrafficMonitor, ctps []*v1alpha1.ClusterTrafficPolicy) *CoverageResult {
	out := &CoverageResult{
		Specs:     map[string]*cppb.MonitoringSpec{},
		TMStatus:  map[types.NamespacedName]*CRStatus{},
		CTPStatus: map[string]*CRStatus{},
	}
	for _, tm := range tms {
		out.TMStatus[types.NamespacedName{Namespace: tm.Namespace, Name: tm.Name}] = &CRStatus{}
	}
	for _, ctp := range ctps {
		out.CTPStatus[ctp.Name] = &CRStatus{}
	}

	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		// Match all TMs (same namespace) and all CTPs (any) for
		// matched-set bookkeeping.
		var matchedTMs []*v1alpha1.TrafficMonitor
		for _, tm := range tms {
			if tm.Namespace != pod.Namespace {
				continue
			}
			sel, err := metaLabelSelectorAsSelector(tm.Spec.WorkloadSelector)
			if err != nil {
				continue
			}
			if sel.Matches(labels.Set(pod.Labels)) {
				matchedTMs = append(matchedTMs, tm)
				key := types.NamespacedName{Namespace: tm.Namespace, Name: tm.Name}
				out.TMStatus[key].MatchedPods = append(out.TMStatus[key].MatchedPods, pod)
			}
		}
		var matchedCTPs []*v1alpha1.ClusterTrafficPolicy
		for _, ctp := range ctps {
			sel := labels.Everything()
			if ctp.Spec.WorkloadSelector != nil {
				s, err := metaLabelSelectorAsSelector(*ctp.Spec.WorkloadSelector)
				if err != nil {
					continue
				}
				sel = s
			}
			if sel.Matches(labels.Set(pod.Labels)) {
				matchedCTPs = append(matchedCTPs, ctp)
				out.CTPStatus[ctp.Name].MatchedPods = append(out.CTPStatus[ctp.Name].MatchedPods, pod)
			}
		}

		// Precedence + tie-break: TMs > CTPs; lex-first wins on tie.
		var winner *coveringResult
		switch {
		case len(matchedTMs) > 0:
			sort.Slice(matchedTMs, func(i, j int) bool { return matchedTMs[i].Name < matchedTMs[j].Name })
			tm := matchedTMs[0]
			winner = &coveringResult{
				MonitoringSpecCore: tm.Spec.MonitoringSpecCore,
				sourceRef:          "TrafficMonitor " + tm.Namespace + "/" + tm.Name,
			}
			key := types.NamespacedName{Namespace: tm.Namespace, Name: tm.Name}
			out.TMStatus[key].CoveredPods = append(out.TMStatus[key].CoveredPods, pod)
			// Conflict surfacing: every other TM that also matched
			// the pod with a disagreeing spec gets logged on both
			// sides.
			for _, other := range matchedTMs[1:] {
				if specsAgree(tm.Spec.MonitoringSpecCore, other.Spec.MonitoringSpecCore) {
					continue
				}
				otherKey := types.NamespacedName{Namespace: other.Namespace, Name: other.Name}
				addConflict(out.TMStatus[key], otherKey.String())
				addConflict(out.TMStatus[otherKey], key.String())
			}
		case len(matchedCTPs) > 0:
			sort.Slice(matchedCTPs, func(i, j int) bool { return matchedCTPs[i].Name < matchedCTPs[j].Name })
			ctp := matchedCTPs[0]
			winner = &coveringResult{
				MonitoringSpecCore: ctp.Spec.MonitoringSpecCore,
				sourceRef:          "ClusterTrafficPolicy " + ctp.Name,
			}
			out.CTPStatus[ctp.Name].CoveredPods = append(out.CTPStatus[ctp.Name].CoveredPods, pod)
			for _, other := range matchedCTPs[1:] {
				if specsAgree(ctp.Spec.MonitoringSpecCore, other.Spec.MonitoringSpecCore) {
					continue
				}
				addConflict(out.CTPStatus[ctp.Name], other.Name)
				addConflict(out.CTPStatus[other.Name], ctp.Name)
			}
		}
		if winner == nil {
			continue
		}
		spec := buildSpecFromCovering(pod, winner)
		if spec == nil {
			continue
		}
		out.Specs[string(pod.UID)] = spec
	}
	return out
}

// buildSpecFromCovering is the single spec-shaping helper: it turns a
// covering MonitoringSpecCore into the wire MonitoringSpec, folding the
// enabled protocols into the bitset and the union of HTTP+gRPC L7 ports
// into HttpPorts. ComputeCoverage (the production reconcile path) and
// ComputeSpec (kept for unit-test stability) both call it, so the two
// never drift on protocol handling. Returns nil if no protocol is
// enabled.
func buildSpecFromCovering(pod *corev1.Pod, cov *coveringResult) *cppb.MonitoringSpec {
	protocols := uint32(0)
	httpPorts := map[int32]struct{}{}
	if cov.Protocols.L4 != nil && cov.Protocols.L4.Enabled {
		protocols |= ProtocolL4TCP
	}
	if cov.Protocols.HTTP != nil && cov.Protocols.HTTP.Enabled {
		protocols |= ProtocolHTTP1
		for _, p := range cov.Protocols.HTTP.Ports {
			httpPorts[p] = struct{}{}
		}
	}
	if cov.Protocols.GRPC != nil && cov.Protocols.GRPC.Enabled {
		protocols |= ProtocolGRPC
		// gRPC and HTTP share the L7 port set on the wire: OBI attaches
		// uprobes per port and detects gRPC vs plaintext HTTP from the
		// content-type, not the port (ADR-0031). So gRPC ports fold into
		// the same instrumented-port set carried as HttpPorts.
		for _, p := range cov.Protocols.GRPC.Ports {
			httpPorts[p] = struct{}{}
		}
	}
	if protocols == 0 {
		return nil
	}
	ports := make([]uint32, 0, len(httpPorts))
	for p := range httpPorts {
		ports = append(ports, uint32(p))
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return &cppb.MonitoringSpec{
		PodUid:    string(pod.UID),
		PodName:   pod.Name,
		Namespace: pod.Namespace,
		NodeName:  pod.Spec.NodeName,
		Protocols: protocols,
		HttpPorts: ports,
		SourceRef: cov.sourceRef,
	}
}

func specsAgree(a, b v1alpha1.MonitoringSpecCore) bool {
	// Two MonitoringSpecCores "agree" iff the same protocols are
	// enabled with the same port sets. Conservative: any
	// difference (different ports for HTTP, L4 enabled in one but
	// not the other) is a conflict.
	if l4Enabled(a) != l4Enabled(b) {
		return false
	}
	if httpEnabled(a) != httpEnabled(b) {
		return false
	}
	if httpEnabled(a) {
		ap := normPorts(a.Protocols.HTTP.Ports)
		bp := normPorts(b.Protocols.HTTP.Ports)
		if len(ap) != len(bp) {
			return false
		}
		for i := range ap {
			if ap[i] != bp[i] {
				return false
			}
		}
	}
	if grpcEnabled(a) != grpcEnabled(b) {
		return false
	}
	if grpcEnabled(a) {
		ap := normPorts(a.Protocols.GRPC.Ports)
		bp := normPorts(b.Protocols.GRPC.Ports)
		if len(ap) != len(bp) {
			return false
		}
		for i := range ap {
			if ap[i] != bp[i] {
				return false
			}
		}
	}
	return true
}

func l4Enabled(c v1alpha1.MonitoringSpecCore) bool {
	return c.Protocols.L4 != nil && c.Protocols.L4.Enabled
}

func httpEnabled(c v1alpha1.MonitoringSpecCore) bool {
	return c.Protocols.HTTP != nil && c.Protocols.HTTP.Enabled
}

func grpcEnabled(c v1alpha1.MonitoringSpecCore) bool {
	return c.Protocols.GRPC != nil && c.Protocols.GRPC.Enabled
}

func normPorts(ps []int32) []int32 {
	out := make([]int32, len(ps))
	copy(out, ps)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func addConflict(s *CRStatus, name string) {
	for _, n := range s.Conflicts {
		if n == name {
			return
		}
	}
	s.Conflicts = append(s.Conflicts, name)
}
