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

package webhook

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func tm(name, ns string, sel metav1.LabelSelector, protos v1alpha1.ProtocolSet) *v1alpha1.TrafficMonitor {
	return &v1alpha1.TrafficMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.TrafficMonitorSpec{
			WorkloadSelector:   sel,
			MonitoringSpecCore: v1alpha1.MonitoringSpecCore{Protocols: protos},
		},
	}
}

func httpOn(ports ...int32) v1alpha1.ProtocolSet {
	return v1alpha1.ProtocolSet{HTTP: &v1alpha1.HTTPConfig{Enabled: true, Ports: ports}}
}

func grpcOn(ports ...int32) v1alpha1.ProtocolSet {
	return v1alpha1.ProtocolSet{GRPC: &v1alpha1.GRPCConfig{Enabled: true, Ports: ports}}
}

func pod(name, ns string, lbls map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbls}}
}

func hasWarning(w []string, substr string) bool {
	for _, s := range w {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func TestValidateCoreRejections(t *testing.T) {
	v := &TrafficMonitorValidator{} // nil client: pure checks only
	ctx := t.Context()

	// Duplicate ports rejected (#90 acceptance: overlapping ports).
	if _, err := v.ValidateCreate(ctx, tm("dup", "ns", metav1.LabelSelector{}, httpOn(8080, 8080))); err == nil {
		t.Error("duplicate http ports accepted")
	}
	// Out-of-range port rejected.
	if _, err := v.ValidateCreate(ctx, tm("range", "ns", metav1.LabelSelector{}, httpOn(0))); err == nil {
		t.Error("port 0 accepted")
	}
	// Invalid selector (bad operator) rejected — the OpenAPI schema
	// cannot check matchExpressions coherence.
	badSel := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "app", Operator: "BogusOp", Values: []string{"x"}},
	}}
	if _, err := v.ValidateCreate(ctx, tm("badsel", "ns", badSel, httpOn(80))); err == nil {
		t.Error("invalid selector operator accepted")
	}
	// Exists with values is also incoherent.
	badSel2 := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "app", Operator: metav1.LabelSelectorOpExists, Values: []string{"x"}},
	}}
	if _, err := v.ValidateCreate(ctx, tm("badsel2", "ns", badSel2, httpOn(80))); err == nil {
		t.Error("Exists-with-values selector accepted")
	}
	// Valid spec passes.
	if _, err := v.ValidateCreate(ctx, tm("ok", "ns", metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}, httpOn(80, 8080))); err != nil {
		t.Errorf("valid TrafficMonitor rejected: %v", err)
	}

	// gRPC ports get the same range/dup checks as HTTP (#105).
	if _, err := v.ValidateCreate(ctx, tm("gdup", "ns", metav1.LabelSelector{}, grpcOn(9090, 9090))); err == nil {
		t.Error("duplicate grpc ports accepted")
	}
	if _, err := v.ValidateCreate(ctx, tm("grange", "ns", metav1.LabelSelector{}, grpcOn(70000))); err == nil {
		t.Error("out-of-range grpc port accepted")
	}
	if _, err := v.ValidateCreate(ctx, tm("gok", "ns", metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}, grpcOn(9090))); err != nil {
		t.Errorf("valid gRPC TrafficMonitor rejected: %v", err)
	}
}

func TestValidateWarnings(t *testing.T) {
	ctx := t.Context()

	// No protocol enabled: accepted with a warning.
	v := &TrafficMonitorValidator{}
	w, err := v.ValidateCreate(ctx, tm("noop", "ns", metav1.LabelSelector{}, v1alpha1.ProtocolSet{}))
	if err != nil {
		t.Fatalf("no-protocol monitor rejected: %v", err)
	}
	if !hasWarning(w, "no protocol is enabled") {
		t.Errorf("missing no-protocol warning, got %v", w)
	}

	// Ports set but http disabled: warning.
	w, err = v.ValidateCreate(ctx, tm("off", "ns", metav1.LabelSelector{},
		v1alpha1.ProtocolSet{HTTP: &v1alpha1.HTTPConfig{Enabled: false, Ports: []int32{80}}}))
	if err != nil {
		t.Fatalf("disabled-http-with-ports rejected: %v", err)
	}
	if !hasWarning(w, "no effect") {
		t.Errorf("missing ports-without-enabled warning, got %v", w)
	}

	// gRPC ports set but grpc disabled: same warning on the grpc path.
	w, err = v.ValidateCreate(ctx, tm("goff", "ns", metav1.LabelSelector{},
		v1alpha1.ProtocolSet{GRPC: &v1alpha1.GRPCConfig{Enabled: false, Ports: []int32{9090}}}))
	if err != nil {
		t.Fatalf("disabled-grpc-with-ports rejected: %v", err)
	}
	if !hasWarning(w, "protocols.grpc.ports") {
		t.Errorf("missing grpc ports-without-enabled warning, got %v", w)
	}
}

func TestValidateClientWarnings(t *testing.T) {
	ctx := t.Context()
	scheme := newScheme(t)
	sel := metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}
	other := tm("other", "ns", sel, httpOn(80))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		pod("shop-1", "ns", map[string]string{"app": "shop"}),
		other,
	).Build()
	v := &TrafficMonitorValidator{Client: cl}

	// Selector matching zero pods: accepted with a warning (#90
	// acceptance).
	w, err := v.ValidateCreate(ctx, tm("zero", "ns", metav1.LabelSelector{MatchLabels: map[string]string{"app": "ghost"}}, httpOn(80)))
	if err != nil {
		t.Fatalf("zero-match monitor rejected: %v", err)
	}
	if !hasWarning(w, "matches no pods") {
		t.Errorf("missing zero-match warning, got %v", w)
	}

	// Overlapping another TM's selection: accepted with a warning
	// naming the other CR (control-plane.md §3: warn, don't reject).
	w, err = v.ValidateCreate(ctx, tm("overlap", "ns", sel, httpOn(80)))
	if err != nil {
		t.Fatalf("overlapping monitor rejected: %v", err)
	}
	if !hasWarning(w, "other") {
		t.Errorf("missing overlap warning naming the sibling, got %v", w)
	}

	// A matching selector with no siblings: no spurious warnings.
	_ = cl.Delete(ctx, other)
	w, err = v.ValidateCreate(ctx, tm("clean", "ns", sel, httpOn(80)))
	if err != nil {
		t.Fatalf("clean monitor rejected: %v", err)
	}
	if len(w) != 0 {
		t.Errorf("unexpected warnings: %v", w)
	}
}

func TestValidateClusterTrafficPolicy(t *testing.T) {
	ctx := t.Context()
	scheme := newScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		pod("shop-1", "prod", map[string]string{"app": "shop"}),
	).Build()
	v := &ClusterTrafficPolicyValidator{Client: cl}

	ctp := func(name string, nsSel, wSel *metav1.LabelSelector) *v1alpha1.ClusterTrafficPolicy {
		return &v1alpha1.ClusterTrafficPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.ClusterTrafficPolicySpec{
				NamespaceSelector:  nsSel,
				WorkloadSelector:   wSel,
				MonitoringSpecCore: v1alpha1.MonitoringSpecCore{Protocols: httpOn(80)},
			},
		}
	}

	// Invalid namespaceSelector rejected.
	bad := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "team", Operator: "Nope"},
	}}
	if _, err := v.ValidateCreate(ctx, ctp("bad", bad, nil)); err == nil {
		t.Error("invalid namespaceSelector accepted")
	}

	// Zero-match workload selector warns, accepted.
	w, err := v.ValidateCreate(ctx, ctp("zero", nil, &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ghost"}}))
	if err != nil {
		t.Fatalf("zero-match CTP rejected: %v", err)
	}
	if !hasWarning(w, "matches no pods") {
		t.Errorf("missing zero-match warning, got %v", w)
	}

	// Matching selector: clean.
	w, err = v.ValidateCreate(ctx, ctp("ok", nil, &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}))
	if err != nil {
		t.Fatalf("valid CTP rejected: %v", err)
	}
	if len(w) != 0 {
		t.Errorf("unexpected warnings: %v", w)
	}
}

// The delete path never blocks.
func TestValidateDelete(t *testing.T) {
	ctx := t.Context()
	if _, err := (&TrafficMonitorValidator{}).ValidateDelete(ctx, &v1alpha1.TrafficMonitor{}); err != nil {
		t.Errorf("TM delete blocked: %v", err)
	}
	if _, err := (&ClusterTrafficPolicyValidator{}).ValidateDelete(ctx, &v1alpha1.ClusterTrafficPolicy{}); err != nil {
		t.Errorf("CTP delete blocked: %v", err)
	}
}
