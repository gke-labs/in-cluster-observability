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
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
	"github.com/gke-labs/in-cluster-observability/pkg/controller/reconciler"
)

// TestComputeCoverage_BasicRollup confirms one CR + one matching pod
// gives a Specs entry, a CoveredPods entry, and no conflicts.
func TestComputeCoverage_BasicRollup(t *testing.T) {
	p := pod("u1", "nginx-1", "demo", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("nginx-monitor", "demo", map[string]string{"app": "nginx"}, http1Core(8080))

	cov := reconciler.ComputeCoverage([]*corev1.Pod{p}, []*v1alpha1.TrafficMonitor{t1}, nil)
	if _, ok := cov.Specs[string(p.UID)]; !ok {
		t.Errorf("Specs missing entry for pod %s", p.UID)
	}
	st := cov.TMStatus[types.NamespacedName{Namespace: "demo", Name: "nginx-monitor"}]
	if st == nil {
		t.Fatal("TMStatus missing entry")
	}
	if len(st.MatchedPods) != 1 {
		t.Errorf("MatchedPods = %d; want 1", len(st.MatchedPods))
	}
	if len(st.CoveredPods) != 1 {
		t.Errorf("CoveredPods = %d; want 1", len(st.CoveredPods))
	}
	if len(st.Conflicts) != 0 {
		t.Errorf("Conflicts = %v; want empty", st.Conflicts)
	}
}

// TestComputeCoverage_ConflictDetection: two TMs select the same
// pod with disagreeing protocol settings → both gain Conflict
// entries pointing at each other.
func TestComputeCoverage_ConflictDetection(t *testing.T) {
	p := pod("u2", "nginx-2", "demo", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("aaaa", "demo", map[string]string{"app": "nginx"}, http1Core(8080))
	t2 := tm("zzzz", "demo", map[string]string{"app": "nginx"}, http1Core(9090))

	cov := reconciler.ComputeCoverage([]*corev1.Pod{p}, []*v1alpha1.TrafficMonitor{t1, t2}, nil)
	a := cov.TMStatus[types.NamespacedName{Namespace: "demo", Name: "aaaa"}]
	z := cov.TMStatus[types.NamespacedName{Namespace: "demo", Name: "zzzz"}]
	if a == nil || z == nil {
		t.Fatal("missing TMStatus entries")
	}
	wantA := []string{"demo/zzzz"}
	wantZ := []string{"demo/aaaa"}
	sort.Strings(a.Conflicts)
	sort.Strings(z.Conflicts)
	if !stringSliceEq(a.Conflicts, wantA) {
		t.Errorf("aaaa.Conflicts = %v; want %v", a.Conflicts, wantA)
	}
	if !stringSliceEq(z.Conflicts, wantZ) {
		t.Errorf("zzzz.Conflicts = %v; want %v", z.Conflicts, wantZ)
	}
	// aaaa wins coverage (lex-first); zzzz does not.
	if len(a.CoveredPods) != 1 {
		t.Errorf("aaaa.CoveredPods = %d; want 1 (lex-first wins)", len(a.CoveredPods))
	}
	if len(z.CoveredPods) != 0 {
		t.Errorf("zzzz.CoveredPods = %d; want 0", len(z.CoveredPods))
	}
}

// TestComputeCoverage_AgreeingSpecsAreNotConflicts: two TMs that
// produce byte-identical protocol settings on the same pod are NOT
// flagged as a conflict (one of them "wins" coverage; both still
// match the pod, but agreement means no operator-actionable
// problem).
func TestComputeCoverage_AgreeingSpecsAreNotConflicts(t *testing.T) {
	p := pod("u3", "nginx-3", "demo", "node-a", map[string]string{"app": "nginx"})
	t1 := tm("aaaa", "demo", map[string]string{"app": "nginx"}, http1Core(80))
	t2 := tm("bbbb", "demo", map[string]string{"app": "nginx"}, http1Core(80))

	cov := reconciler.ComputeCoverage([]*corev1.Pod{p}, []*v1alpha1.TrafficMonitor{t1, t2}, nil)
	for _, st := range cov.TMStatus {
		if len(st.Conflicts) != 0 {
			t.Errorf("Conflicts = %v; want empty (specs agree)", st.Conflicts)
		}
	}
}

// TestComputeCoverage_GRPCReachesDispatch is the regression guard for
// the Phase-3 review finding: the gRPC toggle must be honored on the
// PRODUCTION reconcile path (ComputeCoverage → buildSpecFromCovering),
// not only in the test-only ComputeSpec. A gRPC-only monitor must yield
// a dispatched spec with the ProtocolGRPC bit and its port folded into
// HttpPorts; a mixed HTTP+gRPC monitor must carry both bits and the
// deduped union of ports.
func TestComputeCoverage_GRPCReachesDispatch(t *testing.T) {
	// gRPC-only: previously produced protocols==0 → nil spec → the pod
	// was dropped from dispatch entirely.
	pg := pod("g1", "echo-1", "demo", "node-a", map[string]string{"app": "echo"})
	tg := tm("grpc-only", "demo", map[string]string{"app": "echo"}, v1alpha1.MonitoringSpecCore{
		Protocols: v1alpha1.ProtocolSet{
			GRPC: &v1alpha1.GRPCConfig{Enabled: true, Ports: []int32{9090}},
		},
	})
	cov := reconciler.ComputeCoverage([]*corev1.Pod{pg}, []*v1alpha1.TrafficMonitor{tg}, nil)
	spec := cov.Specs[string(pg.UID)]
	if spec == nil {
		t.Fatal("gRPC-only monitor produced no dispatched spec (feature dead on the production path)")
	}
	if spec.GetProtocols() != reconciler.ProtocolGRPC {
		t.Errorf("Protocols = %b; want %b (grpc only)", spec.GetProtocols(), reconciler.ProtocolGRPC)
	}
	if p := spec.GetHttpPorts(); len(p) != 1 || p[0] != 9090 {
		t.Errorf("HttpPorts = %v; want [9090] (gRPC port folded in)", p)
	}

	// Mixed HTTP+gRPC: both bits set, overlapping port deduped.
	pm := pod("g2", "echo-2", "demo", "node-a", map[string]string{"app": "mix"})
	tmx := tm("mixed", "demo", map[string]string{"app": "mix"}, v1alpha1.MonitoringSpecCore{
		Protocols: v1alpha1.ProtocolSet{
			HTTP: &v1alpha1.HTTPConfig{Enabled: true, Ports: []int32{8080}},
			GRPC: &v1alpha1.GRPCConfig{Enabled: true, Ports: []int32{9090, 8080}},
		},
	})
	cov = reconciler.ComputeCoverage([]*corev1.Pod{pm}, []*v1alpha1.TrafficMonitor{tmx}, nil)
	spec = cov.Specs[string(pm.UID)]
	if spec == nil {
		t.Fatal("mixed HTTP+gRPC monitor produced no dispatched spec")
	}
	if want := reconciler.ProtocolHTTP1 | reconciler.ProtocolGRPC; spec.GetProtocols() != want {
		t.Errorf("Protocols = %b; want %b (http+grpc)", spec.GetProtocols(), want)
	}
	if p := spec.GetHttpPorts(); len(p) != 2 || p[0] != 8080 || p[1] != 9090 {
		t.Errorf("HttpPorts = %v; want [8080 9090] (union, deduped, sorted)", p)
	}
}

// TestComputeCoverage_GRPCConflictDetection: two TMs covering the same
// pod that differ ONLY in gRPC config must be flagged as conflicting —
// specsAgree has to compare gRPC, not just L4/HTTP.
func TestComputeCoverage_GRPCConflictDetection(t *testing.T) {
	p := pod("g3", "echo-3", "demo", "node-a", map[string]string{"app": "echo"})
	base := func(grpcPorts ...int32) v1alpha1.MonitoringSpecCore {
		return v1alpha1.MonitoringSpecCore{
			Protocols: v1alpha1.ProtocolSet{
				HTTP: &v1alpha1.HTTPConfig{Enabled: true, Ports: []int32{8080}},
				GRPC: &v1alpha1.GRPCConfig{Enabled: true, Ports: grpcPorts},
			},
		}
	}
	t1 := tm("aaaa", "demo", map[string]string{"app": "echo"}, base(9090))
	t2 := tm("zzzz", "demo", map[string]string{"app": "echo"}, base(9091))

	cov := reconciler.ComputeCoverage([]*corev1.Pod{p}, []*v1alpha1.TrafficMonitor{t1, t2}, nil)
	a := cov.TMStatus[types.NamespacedName{Namespace: "demo", Name: "aaaa"}]
	z := cov.TMStatus[types.NamespacedName{Namespace: "demo", Name: "zzzz"}]
	if a == nil || z == nil {
		t.Fatal("missing TMStatus entries")
	}
	if !stringSliceEq(a.Conflicts, []string{"demo/zzzz"}) {
		t.Errorf("aaaa.Conflicts = %v; want [demo/zzzz] (gRPC ports differ)", a.Conflicts)
	}
	if !stringSliceEq(z.Conflicts, []string{"demo/aaaa"}) {
		t.Errorf("zzzz.Conflicts = %v; want [demo/aaaa]", z.Conflicts)
	}
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
