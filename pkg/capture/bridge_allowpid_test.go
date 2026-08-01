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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
)

// readConfigSettling polls the OBI config file until it contains the
// expected substring or the deadline elapses. Used because the
// coalescer writes asynchronously after the debounce window.
func readConfigSettling(t *testing.T, path, want string, deadline time.Duration) string {
	t.Helper()
	end := time.Now().Add(deadline)
	var last string
	for time.Now().Before(end) {
		b, err := os.ReadFile(path)
		if err == nil {
			last = string(b)
			if want == "" || strings.Contains(last, want) {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

// readConfigGoneSettling polls the OBI config file until the given
// substring disappears or the deadline elapses. Counterpart of
// readConfigSettling for waiting on debounced removals.
func readConfigGoneSettling(t *testing.T, path, gone string, deadline time.Duration) string {
	t.Helper()
	end := time.Now().Add(deadline)
	var last string
	for time.Now().Before(end) {
		b, err := os.ReadFile(path)
		if err == nil {
			last = string(b)
			if !strings.Contains(last, gone) {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

func TestAllowPID_WritesDiscoveryService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, err := capture.NewBridge(capture.Config{
		ObiConfigPath: path,
		OBIEndpoint:   "127.0.0.1:4317",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop(t.Context())

	if err := mgr.AllowPID(12345, capture.PIDSpec{
		Protocols: []capture.Module{capture.ModuleHTTP1},
	}); err != nil {
		t.Fatalf("AllowPID: %v", err)
	}

	content := readConfigSettling(t, path, "pid-12345", 3*time.Second)
	if !strings.Contains(content, "pid-12345") {
		t.Errorf("AllowPID should add a discovery service named pid-12345; got:\n%s", content)
	}
}

func TestBlockPID_RemovesDiscoveryService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, err := capture.NewBridge(capture.Config{
		ObiConfigPath: path,
		OBIEndpoint:   "127.0.0.1:4317",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop(t.Context())

	_ = mgr.AllowPID(12345, capture.PIDSpec{})
	_ = readConfigSettling(t, path, "pid-12345", 3*time.Second)
	_ = mgr.BlockPID(12345)
	// The removal lands asynchronously after the debounce window, so
	// poll for the entry to disappear rather than sleeping a fixed
	// interval (a 700ms sleep vs the 500ms debounce flaked on slow
	// CI runners).
	content := readConfigGoneSettling(t, path, "pid-12345", 3*time.Second)
	if strings.Contains(content, "pid-12345") {
		t.Errorf("BlockPID should remove discovery service; got:\n%s", content)
	}
}

func TestAllowPID_Coalescing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, err := capture.NewBridge(capture.Config{
		ObiConfigPath: path,
		OBIEndpoint:   "127.0.0.1:4317",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop(t.Context())

	// Burst: 100 AllowPIDs in a tight loop. The coalescer should produce
	// a single write at the end, not 100.
	for i := uint32(0); i < 100; i++ {
		if err := mgr.AllowPID(1000+i, capture.PIDSpec{}); err != nil {
			t.Fatalf("AllowPID(%d): %v", i, err)
		}
	}
	// Wait for the debounce to elapse + write to complete.
	content := readConfigSettling(t, path, "pid-1099", 3*time.Second)
	if !strings.Contains(content, "pid-1099") {
		t.Errorf("expected final PID present after coalesced write; got:\n%s", content)
	}
	if !strings.Contains(content, "pid-1000") {
		t.Errorf("expected first PID present after coalesced write; got:\n%s", content)
	}
}

func TestInitialOpenPorts_SeedsDiscoveryAtStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, err := capture.NewBridge(capture.Config{
		ObiConfigPath:    path,
		OBIEndpoint:      "http://127.0.0.1:4318",
		InitialOpenPorts: "80,8080",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop(t.Context())

	content := readConfigSettling(t, path, "open_ports: 80,8080", 2*time.Second)
	if !strings.Contains(content, "instrument:") {
		t.Errorf("expected `instrument:` selector key; got:\n%s", content)
	}
	if !strings.Contains(content, "name: smoke") {
		t.Errorf("expected synthetic `name: smoke` entry; got:\n%s", content)
	}
}

func TestInitialOpenPorts_DisplacedByAllowPID(t *testing.T) {
	// Once any AllowPID arrives, per-PID Instrument entries take
	// over and the smoke seed disappears.
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, err := capture.NewBridge(capture.Config{
		ObiConfigPath:    path,
		OBIEndpoint:      "http://127.0.0.1:4318",
		InitialOpenPorts: "80",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop(t.Context())

	_ = readConfigSettling(t, path, "name: smoke", 2*time.Second)
	_ = mgr.AllowPID(2024, capture.PIDSpec{})
	content := readConfigSettling(t, path, "target_pids:", 3*time.Second)
	if strings.Contains(content, "name: smoke") {
		t.Errorf("AllowPID should replace the smoke seed; still present in:\n%s", content)
	}
	if !strings.Contains(content, "2024") {
		t.Errorf("expected PID 2024 in target_pids; got:\n%s", content)
	}
}

func TestAllowPID_IdempotentDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	mgr, err := capture.NewBridge(capture.Config{
		ObiConfigPath: path,
		OBIEndpoint:   "127.0.0.1:4317",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop(t.Context())

	spec := capture.PIDSpec{}
	_ = mgr.AllowPID(42, spec)
	_ = readConfigSettling(t, path, "pid-42", 3*time.Second)

	stat1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mtime1 := stat1.ModTime()

	// Repeat AllowPID with identical content. The writer short-circuits
	// on unchanged content, so the file should not be re-written.
	_ = mgr.AllowPID(42, spec)
	time.Sleep(700 * time.Millisecond) // longer than debounce
	stat2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !stat2.ModTime().Equal(mtime1) {
		t.Errorf("identical AllowPID should not rewrite file; mtime changed from %v to %v", mtime1, stat2.ModTime())
	}
}
