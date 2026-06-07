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

package capture_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
)

// TestAllowPod_WritesK8sMetadataInstrument confirms AllowPod
// produces an obiconfig.Instrument entry with k8s_pod_name +
// k8s_namespace (not target_pids). This is the v0.4 controller-
// driven path; the v0.3 pseudo-PID approach is gone.
func TestAllowPod_WritesK8sMetadataInstrument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, err := capture.NewBridge(capture.Config{
		ObiConfigPath: path,
		OBIEndpoint:   "http://127.0.0.1:4318",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop(context.Background())

	if err := mgr.AllowPod("abc12345-pod-uid", capture.PodSpec{
		PodName:   "nginx-567b68cc5f-6mggl",
		Namespace: "demo",
		HTTPPorts: []uint16{80, 8080},
	}); err != nil {
		t.Fatalf("AllowPod: %v", err)
	}

	content := readConfigSettling(t, path, "k8s_pod_name: nginx-567b68cc5f-6mggl", 3*time.Second)
	if !strings.Contains(content, "k8s_namespace: demo") {
		t.Errorf("expected k8s_namespace match; got:\n%s", content)
	}
	if !strings.Contains(content, "open_ports: 80,8080") {
		t.Errorf("expected open_ports: 80,8080 string-form; got:\n%s", content)
	}
	// target_pids was the old pseudo-PID path; v0.4 controller-
	// driven entries must NOT carry it. (PID-driven AllowPID
	// entries still use it; the agent's debug endpoint relies on
	// that path. Just not for pods.)
	if strings.Contains(content, "target_pids:") {
		t.Errorf("AllowPod entry should not carry target_pids; got:\n%s", content)
	}
}

// TestAllowPod_NameDeterministicAcrossReloads pins the
// pod-NAME → Instrument.Name shape (`pod-` + first 12 of UID).
// Changing this is a wire-visible change in the on-disk OBI config.
func TestAllowPod_NameDeterministicAcrossReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, _ := capture.NewBridge(capture.Config{
		ObiConfigPath: path,
		OBIEndpoint:   "http://127.0.0.1:4318",
	})
	_ = mgr.Start(context.Background())
	defer mgr.Stop(context.Background())

	_ = mgr.AllowPod("abc12345-6789-defg-hijk-lmnopqrstuvw", capture.PodSpec{
		PodName: "nginx", Namespace: "demo",
	})
	content := readConfigSettling(t, path, "name: pod-abc12345-678", 3*time.Second)
	if !strings.Contains(content, "name: pod-abc12345-678") {
		t.Errorf("expected `name: pod-abc12345-678` (first 12 of UID); got:\n%s", content)
	}
}

// TestBlockPod_RemovesEntry: after BlockPod the entry vanishes from
// the rendered OBI config.
func TestBlockPod_RemovesEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, _ := capture.NewBridge(capture.Config{
		ObiConfigPath: path,
		OBIEndpoint:   "http://127.0.0.1:4318",
	})
	_ = mgr.Start(context.Background())
	defer mgr.Stop(context.Background())

	_ = mgr.AllowPod("xx-uid", capture.PodSpec{PodName: "x", Namespace: "ns"})
	_ = readConfigSettling(t, path, "k8s_pod_name: x", 3*time.Second)

	_ = mgr.BlockPod("xx-uid")
	// Wait a debounce-window past the trigger so the writer settles.
	time.Sleep(700 * time.Millisecond)
	content := readConfigSettling(t, path, "", 3*time.Second)
	if strings.Contains(content, "k8s_pod_name: x") {
		t.Errorf("BlockPod should remove the entry; got:\n%s", content)
	}
}

// TestAllowPod_EmptyUIDIsRejected: an empty UID is a programmer
// error (the dispatcher's contract says every MonitoringSpec has a
// PodUid) and would conflate distinct pods if accepted.
func TestAllowPod_EmptyUIDIsRejected(t *testing.T) {
	mgr, _ := capture.NewBridge(capture.Config{
		OBIEndpoint: "http://127.0.0.1:4318",
	})
	_ = mgr.Start(context.Background())
	defer mgr.Stop(context.Background())

	if err := mgr.AllowPod("", capture.PodSpec{}); err == nil {
		t.Error("AllowPod with empty UID should fail; got nil err")
	}
}

// TestAllowPod_SmokeSeedDisplaced confirms once any AllowPod arrives,
// the --obi-instrument-ports seed entry is no longer emitted (same
// displacement rule as AllowPID).
func TestAllowPod_SmokeSeedDisplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, _ := capture.NewBridge(capture.Config{
		ObiConfigPath:    path,
		OBIEndpoint:      "http://127.0.0.1:4318",
		InitialOpenPorts: "80",
	})
	_ = mgr.Start(context.Background())
	defer mgr.Stop(context.Background())

	_ = readConfigSettling(t, path, "name: smoke", 2*time.Second)
	_ = mgr.AllowPod("uid-1", capture.PodSpec{PodName: "n", Namespace: "demo"})
	content := readConfigSettling(t, path, "k8s_pod_name: n", 3*time.Second)
	if strings.Contains(content, "name: smoke") {
		t.Errorf("smoke seed should be displaced; still present in:\n%s", content)
	}
}
