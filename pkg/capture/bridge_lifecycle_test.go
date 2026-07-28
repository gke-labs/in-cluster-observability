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

package capture

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
)

// Regression tests for the #154 lifecycle bugs.

// TestStopAfterFailedStart verifies Stop returns promptly when Start
// failed before launching the coalescer. Before #154 this deadlocked:
// Start set started=true, failed on the receiver bind, and Stop then
// blocked forever on <-coalDone.
func TestStopAfterFailedStart(t *testing.T) {
	// Occupy a port so the receiver bind fails.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	mgr, err := NewBridge(Config{
		OTLPGRPCAddr:  l.Addr().String(),
		ObiConfigPath: filepath.Join(t.TempDir(), "obi.yaml"),
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := mgr.Start(context.Background()); err == nil {
		t.Fatalf("Start succeeded on an occupied port; want error")
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- mgr.Stop(ctx)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop after failed Start: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Stop blocked after failed Start (coalDone deadlock)")
	}
}

// TestEmitAfterStopIsSafe verifies that OTLP handler callbacks and
// restart reports arriving after Stop are dropped instead of sending
// on the closed events channel. Before #154 this was a
// send-on-closed-channel panic — and when the send happened inside
// recoverPanic's markDegraded, an unrecoverable double panic.
func TestEmitAfterStopIsSafe(t *testing.T) {
	mgr, err := NewBridge(Config{ObiConfigPath: filepath.Join(t.TempDir(), "obi.yaml")})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	b := mgr.(*bridgeManager)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := mgr.EnableModule(ModuleHTTP1, ModuleConfig{}); err != nil {
		t.Fatalf("EnableModule: %v", err)
	}
	if err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Handler callback after Stop: must be a silent drop.
	h := &bridgeHandler{b: b}
	if err := h.OnMetrics(context.Background(), &collmetricspb.ExportMetricsServiceRequest{}); err != nil {
		t.Fatalf("OnMetrics after Stop: %v", err)
	}
	// Restart report after Stop (threshold crossed, module enabled —
	// the markDegraded path): must be a silent drop.
	b.ReportOBIRestart(context.Background(), 5)
}

// TestConcurrentStartStop exercises Start/Stop races under the race
// detector. Before #154, Start assigned b.receiver outside the mutex
// that Stop read it under.
func TestConcurrentStartStop(t *testing.T) {
	for i := 0; i < 20; i++ {
		mgr, err := NewBridge(Config{
			OTLPGRPCAddr:  "127.0.0.1:0",
			ObiConfigPath: filepath.Join(t.TempDir(), "obi.yaml"),
		})
		if err != nil {
			t.Fatalf("NewBridge: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = mgr.Start(context.Background())
		}()
		go func() {
			defer wg.Done()
			_ = mgr.Stop(context.Background())
		}()
		wg.Wait()
		// Whatever interleaving happened, a final Stop must not hang.
		done := make(chan struct{})
		go func() {
			_ = mgr.Stop(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("iteration %d: final Stop hung", i)
		}
	}
}

// TestReportOBIRestartDelta verifies the counter tracks the cumulative
// restart count, not the call count (#154).
func TestReportOBIRestartDelta(t *testing.T) {
	mgr, err := NewBridge(Config{})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	b := mgr.(*bridgeManager)

	b.ReportOBIRestart(context.Background(), 2)
	if got := b.lastRestartCount; got != 2 {
		t.Fatalf("after report(2): lastRestartCount = %d, want 2", got)
	}
	// Same count again — no advance.
	b.ReportOBIRestart(context.Background(), 2)
	if got := b.lastRestartCount; got != 2 {
		t.Fatalf("after repeated report(2): lastRestartCount = %d, want 2", got)
	}
	// Kubelet counter reset (pod recreated) — counts lower than the
	// high-water mark don't regress it.
	b.ReportOBIRestart(context.Background(), 1)
	if got := b.lastRestartCount; got != 2 {
		t.Fatalf("after report(1): lastRestartCount = %d, want 2", got)
	}
	b.ReportOBIRestart(context.Background(), 6)
	if got := b.lastRestartCount; got != 6 {
		t.Fatalf("after report(6): lastRestartCount = %d, want 6", got)
	}
}
