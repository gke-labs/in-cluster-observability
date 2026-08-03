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

// Package webhook implements the validating admission webhook for
// TrafficMonitor and ClusterTrafficPolicy (#90, ADR-0030,
// control-plane.md §5).
//
// Split of responsibilities:
//   - the CRD OpenAPI schema (controller-gen markers) rejects
//     type/range/enum errors before this code runs;
//   - this webhook rejects semantic errors the schema cannot express
//     (invalid selectors, duplicate ports) and attaches soft WARNINGS
//     for conditions that are legal but probably not what the author
//     meant (no protocol enabled, selector matching zero pods,
//     overlap with another CR) — warnings surface in kubectl output
//     and never block the write, because those states may be
//     transient during rollouts (control-plane.md §3);
//   - cross-resource conflict detection stays in the reconciler
//     (Conflict conditions, ADR-0022.4) as the reactive layer for
//     anything that slips past admission, including the entire
//     bootstrap window while failurePolicy is still Ignore.
//
// Checks for fields that do not exist yet (sampling rate ranges, path
// templating RE2, sink catalogs — #108/#109) slot into validateCore
// when those fields land.
package webhook

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
)

// validateCore checks the shared MonitoringSpecCore. Returned errors
// reject the write; returned warnings are attached to the response.
func validateCore(path *field.Path, core v1alpha1.MonitoringSpecCore) (admission.Warnings, field.ErrorList) {
	var warnings admission.Warnings
	var errs field.ErrorList

	httpEnabled := core.Protocols.HTTP != nil && core.Protocols.HTTP.Enabled
	grpcEnabled := core.Protocols.GRPC != nil && core.Protocols.GRPC.Enabled
	l4Enabled := core.Protocols.L4 != nil && core.Protocols.L4.Enabled
	if !httpEnabled && !grpcEnabled && !l4Enabled {
		warnings = append(warnings, "no protocol is enabled (protocols.l4/protocols.http/protocols.grpc); this monitor selects pods but captures nothing")
	}

	if core.Protocols.HTTP != nil {
		validatePorts(path.Child("protocols", "http"), "http", core.Protocols.HTTP.Ports, core.Protocols.HTTP.Enabled, &warnings, &errs)
	}
	if core.Protocols.GRPC != nil {
		validatePorts(path.Child("protocols", "grpc"), "grpc", core.Protocols.GRPC.Ports, core.Protocols.GRPC.Enabled, &warnings, &errs)
	}

	return warnings, errs
}

// validatePorts checks an L7 protocol's port list: each port in
// [1,65535], no duplicates, and a warning if ports are set while the
// protocol is disabled. The CRD schema enforces the range too; the
// webhook re-checks it for a clear message and to cover the
// failurePolicy=Ignore bootstrap window (Phase 2 review #2).
func validatePorts(protoPath *field.Path, proto string, ports []int32, enabled bool, warnings *admission.Warnings, errs *field.ErrorList) {
	portsPath := protoPath.Child("ports")
	seen := map[int32]bool{}
	for i, p := range ports {
		if p < 1 || p > 65535 {
			*errs = append(*errs, field.Invalid(portsPath.Index(i), p, "port must be in 1-65535"))
			continue
		}
		if seen[p] {
			*errs = append(*errs, field.Duplicate(portsPath.Index(i), p))
		}
		seen[p] = true
	}
	if len(ports) > 0 && !enabled {
		*warnings = append(*warnings, fmt.Sprintf("protocols.%s.ports is set but protocols.%s.enabled is false; the ports have no effect", proto, proto))
	}
}

// compileSelector converts a LabelSelector, recording a field error on
// failure (the schema cannot validate operator/values coherence).
func compileSelector(path *field.Path, sel *metav1.LabelSelector, errs *field.ErrorList) labels.Selector {
	if sel == nil {
		return labels.Everything()
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		*errs = append(*errs, field.Invalid(path, sel, err.Error()))
		return nil
	}
	return s
}

// TrafficMonitorValidator validates TrafficMonitor admission.
type TrafficMonitorValidator struct {
	// Client reads pods and sibling TrafficMonitors from the
	// manager's cache for the soft warnings. nil skips those checks
	// (unit tests of the pure core).
	Client client.Reader
}

var _ admission.CustomValidator = (*TrafficMonitorValidator)(nil)

// ValidateCreate implements admission.CustomValidator.
func (v *TrafficMonitorValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

// ValidateUpdate implements admission.CustomValidator.
func (v *TrafficMonitorValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

// ValidateDelete implements admission.CustomValidator.
func (v *TrafficMonitorValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *TrafficMonitorValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	tm, ok := obj.(*v1alpha1.TrafficMonitor)
	if !ok {
		return nil, fmt.Errorf("expected a TrafficMonitor, got %T", obj)
	}
	specPath := field.NewPath("spec")
	warnings, errs := validateCore(specPath, tm.Spec.MonitoringSpecCore)
	sel := compileSelector(specPath.Child("workloadSelector"), &tm.Spec.WorkloadSelector, &errs)
	if len(errs) > 0 {
		return warnings, errs.ToAggregate()
	}

	if v.Client != nil && sel != nil {
		// Soft warnings only — a cache hiccup must not block writes.
		var pods corev1.PodList
		if err := v.Client.List(ctx, &pods, client.InNamespace(tm.Namespace), client.MatchingLabelsSelector{Selector: sel}); err == nil {
			if len(pods.Items) == 0 {
				warnings = append(warnings, fmt.Sprintf("workloadSelector currently matches no pods in namespace %s (fine if the workload deploys later)", tm.Namespace))
			}
			if overlaps := overlappingMonitors(ctx, v.Client, tm, pods.Items); len(overlaps) > 0 {
				warnings = append(warnings, fmt.Sprintf("selection overlaps TrafficMonitor(s) %v; the controller resolves overlap deterministically and reports Conflict conditions (control-plane.md §3)", overlaps))
			}
		}
	}
	return warnings, nil
}

// overlappingMonitors names other TrafficMonitors in the namespace
// whose selectors match any of the same pods.
func overlappingMonitors(ctx context.Context, r client.Reader, tm *v1alpha1.TrafficMonitor, matched []corev1.Pod) []string {
	var tms v1alpha1.TrafficMonitorList
	if err := r.List(ctx, &tms, client.InNamespace(tm.Namespace)); err != nil {
		return nil
	}
	var out []string
	for i := range tms.Items {
		other := &tms.Items[i]
		if other.Name == tm.Name {
			continue
		}
		osel, err := metav1.LabelSelectorAsSelector(&other.Spec.WorkloadSelector)
		if err != nil {
			continue
		}
		for j := range matched {
			if osel.Matches(labels.Set(matched[j].Labels)) {
				out = append(out, other.Name)
				break
			}
		}
	}
	return out
}

// ClusterTrafficPolicyValidator validates ClusterTrafficPolicy
// admission.
type ClusterTrafficPolicyValidator struct {
	// Client reads pods from the manager's cache for the zero-match
	// warning. nil skips it.
	Client client.Reader
}

var _ admission.CustomValidator = (*ClusterTrafficPolicyValidator)(nil)

// ValidateCreate implements admission.CustomValidator.
func (v *ClusterTrafficPolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

// ValidateUpdate implements admission.CustomValidator.
func (v *ClusterTrafficPolicyValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

// ValidateDelete implements admission.CustomValidator.
func (v *ClusterTrafficPolicyValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClusterTrafficPolicyValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	ctp, ok := obj.(*v1alpha1.ClusterTrafficPolicy)
	if !ok {
		return nil, fmt.Errorf("expected a ClusterTrafficPolicy, got %T", obj)
	}
	specPath := field.NewPath("spec")
	warnings, errs := validateCore(specPath, ctp.Spec.MonitoringSpecCore)
	compileSelector(specPath.Child("namespaceSelector"), ctp.Spec.NamespaceSelector, &errs)
	wsel := compileSelector(specPath.Child("workloadSelector"), ctp.Spec.WorkloadSelector, &errs)
	if len(errs) > 0 {
		return warnings, errs.ToAggregate()
	}

	if v.Client != nil && wsel != nil {
		// Zero-match is checked on the workload selector cluster-wide;
		// the namespace dimension is left to the reconciler (matching
		// namespaces then intersecting pods is its steady-state job).
		var pods corev1.PodList
		if err := v.Client.List(ctx, &pods, client.MatchingLabelsSelector{Selector: wsel}); err == nil && len(pods.Items) == 0 {
			warnings = append(warnings, "workloadSelector currently matches no pods in any namespace (fine if the workload deploys later)")
		}
	}
	return warnings, nil
}
