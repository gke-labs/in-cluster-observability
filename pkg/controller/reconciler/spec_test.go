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

package reconciler_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
	cppb "github.com/gke-labs/in-cluster-observability/pkg/controller/pb/controlplane/v1"
	"github.com/gke-labs/in-cluster-observability/pkg/controller/reconciler"
)

func pod(uid, name, ns, node string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

func tm(name, ns string, matchLabels map[string]string, core v1alpha1.MonitoringSpecCore) *v1alpha1.TrafficMonitor {
	return &v1alpha1.TrafficMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.TrafficMonitorSpec{
			WorkloadSelector:   metav1.LabelSelector{MatchLabels: matchLabels},
			MonitoringSpecCore: core,
		},
	}
}

func ctp(name string, podSelector *metav1.LabelSelector, core v1alpha1.MonitoringSpecCore) *v1alpha1.ClusterTrafficPolicy {
	return &v1alpha1.ClusterTrafficPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ClusterTrafficPolicySpec{
			WorkloadSelector:   podSelector,
			MonitoringSpecCore: core,
		},
	}
}

func http1Core(ports ...int32) v1alpha1.MonitoringSpecCore {
	return v1alpha1.MonitoringSpecCore{
		Protocols: v1alpha1.ProtocolSet{
			HTTP: &v1alpha1.HTTPConfig{Enabled: true, Ports: ports},
		},
	}
}

// TestComputeSpec_TMOverridesCTP confirms ADR-0003 precedence:
// a namespaced TrafficMonitor wins over a cluster default for
// pods it covers.
func TestComputeSpec_TMOverridesCTP(t *testing.T) {
	p := pod("u1", "nginx-1", "demo", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("nginx-monitor", "demo", map[string]string{"app": "nginx"}, http1Core(8080))
	c1 := ctp("cluster-default", nil, http1Core(80))

	got := reconciler.ComputeSpec(p, []*v1alpha1.TrafficMonitor{t1}, []*v1alpha1.ClusterTrafficPolicy{c1})
	if got == nil {
		t.Fatal("expected a spec; got nil")
	}
	if got.GetSourceRef() != "TrafficMonitor demo/nginx-monitor" {
		t.Errorf("SourceRef = %q; expected the TrafficMonitor (most-specific wins)", got.GetSourceRef())
	}
	if want := []uint32{8080}; len(got.GetHttpPorts()) != 1 || got.GetHttpPorts()[0] != want[0] {
		t.Errorf("HttpPorts = %v; want %v (from TM, not CTP)", got.GetHttpPorts(), want)
	}
}

// TestComputeSpec_CTPFallback covers the cluster-default path when
// no TrafficMonitor matches.
func TestComputeSpec_CTPFallback(t *testing.T) {
	p := pod("u2", "unmatched", "demo", "node-a", map[string]string{"app": "other"})
	c1 := ctp("cluster-default", nil, http1Core(80))

	got := reconciler.ComputeSpec(p, nil, []*v1alpha1.ClusterTrafficPolicy{c1})
	if got == nil {
		t.Fatal("expected fallback spec from CTP")
	}
	if got.GetSourceRef() != "ClusterTrafficPolicy cluster-default" {
		t.Errorf("SourceRef = %q; expected the CTP name", got.GetSourceRef())
	}
}

// TestComputeSpec_NoMatch returns nil when nothing covers the pod.
func TestComputeSpec_NoMatch(t *testing.T) {
	p := pod("u3", "lonely", "demo", "node-a", nil)
	got := reconciler.ComputeSpec(p, nil, nil)
	if got != nil {
		t.Errorf("expected nil; got %+v", got)
	}
}

// TestComputeSpec_NoEnabledProtocol returns nil when a CR matches
// but every protocol toggle is false — the spec would be a no-op
// and dispatching it would produce a useless OBI config entry.
func TestComputeSpec_NoEnabledProtocol(t *testing.T) {
	p := pod("u4", "nginx-2", "demo", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("disabled-monitor", "demo", map[string]string{"app": "nginx"}, v1alpha1.MonitoringSpecCore{
		Protocols: v1alpha1.ProtocolSet{
			HTTP: &v1alpha1.HTTPConfig{Enabled: false},
			L4:   &v1alpha1.L4Config{Enabled: false},
		},
	})
	got := reconciler.ComputeSpec(p, []*v1alpha1.TrafficMonitor{t1}, nil)
	if got != nil {
		t.Errorf("expected nil (no enabled protocol); got %+v", got)
	}
}

// TestComputeSpec_LexFirstOnTie picks the lexicographically-first
// CR when two TrafficMonitors cover the same pod. Deterministic;
// conflict surfacing arrives in Phase 3.
func TestComputeSpec_LexFirstOnTie(t *testing.T) {
	p := pod("u5", "nginx-3", "demo", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("zzzz", "demo", map[string]string{"app": "nginx"}, http1Core(8080))
	t2 := tm("aaaa", "demo", map[string]string{"app": "nginx"}, http1Core(9090))

	got := reconciler.ComputeSpec(p, []*v1alpha1.TrafficMonitor{t1, t2}, nil)
	if got == nil {
		t.Fatal("expected a spec")
	}
	if got.GetSourceRef() != "TrafficMonitor demo/aaaa" {
		t.Errorf("SourceRef = %q; expected aaaa (lex-first wins on tie)", got.GetSourceRef())
	}
}

// TestComputeSpec_NamespaceIsolation confirms a TrafficMonitor in
// namespace A does not cover pods in namespace B.
func TestComputeSpec_NamespaceIsolation(t *testing.T) {
	p := pod("u6", "nginx-4", "payments", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("payments-monitor", "demo", map[string]string{"app": "nginx"}, http1Core(8080))

	got := reconciler.ComputeSpec(p, []*v1alpha1.TrafficMonitor{t1}, nil)
	if got != nil {
		t.Errorf("expected nil (TM is in different namespace); got %+v", got)
	}
}

// TestComputeSpec_ProtocolsBitset confirms the bitset packs L4 +
// HTTP1 into the expected values on the wire.
func TestComputeSpec_ProtocolsBitset(t *testing.T) {
	p := pod("u7", "nginx-5", "demo", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("both", "demo", map[string]string{"app": "nginx"}, v1alpha1.MonitoringSpecCore{
		Protocols: v1alpha1.ProtocolSet{
			L4:   &v1alpha1.L4Config{Enabled: true},
			HTTP: &v1alpha1.HTTPConfig{Enabled: true, Ports: []int32{80}},
		},
	})
	got := reconciler.ComputeSpec(p, []*v1alpha1.TrafficMonitor{t1}, nil)
	if got == nil {
		t.Fatal("expected a spec")
	}
	want := reconciler.ProtocolL4TCP | reconciler.ProtocolHTTP1
	if got.GetProtocols() != want {
		t.Errorf("Protocols bitset = %b; want %b", got.GetProtocols(), want)
	}
}

// TestComputeSpec_GRPCBitsetAndPorts confirms the gRPC toggle ORs
// ProtocolGRPC and folds its ports into the shared L7 port set
// (HttpPorts), deduped and sorted alongside HTTP ports (ADR-0031).
func TestComputeSpec_GRPCBitsetAndPorts(t *testing.T) {
	p := pod("u8", "echo-1", "demo", "node-a", map[string]string{"app": "echo"})
	t1 := tm("l7", "demo", map[string]string{"app": "echo"}, v1alpha1.MonitoringSpecCore{
		Protocols: v1alpha1.ProtocolSet{
			L4:   &v1alpha1.L4Config{Enabled: true},
			HTTP: &v1alpha1.HTTPConfig{Enabled: true, Ports: []int32{8080}},
			GRPC: &v1alpha1.GRPCConfig{Enabled: true, Ports: []int32{9090, 8080}},
		},
	})
	got := reconciler.ComputeSpec(p, []*v1alpha1.TrafficMonitor{t1}, nil)
	if got == nil {
		t.Fatal("expected a spec")
	}
	want := reconciler.ProtocolL4TCP | reconciler.ProtocolHTTP1 | reconciler.ProtocolGRPC
	if got.GetProtocols() != want {
		t.Errorf("Protocols bitset = %b; want %b", got.GetProtocols(), want)
	}
	// 8080 appears in both HTTP and gRPC; must dedupe. Result sorted.
	if wantPorts := []uint32{8080, 9090}; len(got.GetHttpPorts()) != 2 ||
		got.GetHttpPorts()[0] != wantPorts[0] || got.GetHttpPorts()[1] != wantPorts[1] {
		t.Errorf("HttpPorts = %v; want %v (HTTP+gRPC merged, deduped, sorted)", got.GetHttpPorts(), wantPorts)
	}
}

// TestComputeSpec_GRPCOnly confirms a gRPC-only monitor produces a
// spec (regression against the "no protocol enabled → nil" guard
// forgetting gRPC).
func TestComputeSpec_GRPCOnly(t *testing.T) {
	p := pod("u9", "echo-2", "demo", "node-a", map[string]string{"app": "echo"})
	t1 := tm("grpc-only", "demo", map[string]string{"app": "echo"}, v1alpha1.MonitoringSpecCore{
		Protocols: v1alpha1.ProtocolSet{
			GRPC: &v1alpha1.GRPCConfig{Enabled: true, Ports: []int32{9090}},
		},
	})
	got := reconciler.ComputeSpec(p, []*v1alpha1.TrafficMonitor{t1}, nil)
	if got == nil {
		t.Fatal("expected a spec for a gRPC-only monitor")
	}
	if got.GetProtocols() != reconciler.ProtocolGRPC {
		t.Errorf("Protocols bitset = %b; want %b (grpc only)", got.GetProtocols(), reconciler.ProtocolGRPC)
	}
}

// Aid for reading the test: confirm we use the proto types defined
// in pkg/controller/pb/controlplane/v1 (catches accidental import
// drift if the proto regenerates to a new path).
var _ = (*cppb.MonitoringSpec)(nil)
