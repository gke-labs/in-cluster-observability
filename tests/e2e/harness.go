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

// Package e2e holds the Kind-based end-to-end tests for the ollie
// DaemonSet (issue #150, per ADR-0023). Run via `ap e2e .`, which sets
// RUN_E2E=1; without it every test skips. The harness is stdlib-only by
// design — the module's dependency policy (AGENTS.md) applies to test
// code too, and everything here is a thin wrapper over the kind /
// docker / kubectl binaries CI already provides.
package e2e

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Harness drives one Kind cluster for the duration of a test. Every
// kubectl invocation pins --context to this cluster so a stray
// KUBECONFIG default can never route commands at another cluster.
type Harness struct {
	t           *testing.T
	ClusterName string
}

// NewHarness creates the Kind cluster (or reuses an existing one with
// the same name) and registers teardown. Set KEEP_CLUSTER=1 to keep the
// cluster after the test for debugging.
func NewHarness(t *testing.T, clusterName string) *Harness {
	t.Helper()
	h := &Harness{t: t, ClusterName: clusterName}

	out, err := exec.Command("kind", "get", "clusters").Output()
	if err == nil && containsLine(string(out), clusterName) {
		t.Logf("kind cluster %q already exists; reusing", clusterName)
	} else {
		t.Logf("creating kind cluster %q", clusterName)
		h.Run("kind", "create", "cluster", "--name", clusterName, "--wait", "2m")
	}

	t.Cleanup(func() {
		if os.Getenv("KEEP_CLUSTER") != "" {
			t.Logf("KEEP_CLUSTER set; leaving kind cluster %q running", clusterName)
			return
		}
		t.Logf("deleting kind cluster %q", clusterName)
		if out, err := exec.Command("kind", "delete", "cluster", "--name", clusterName).CombinedOutput(); err != nil {
			t.Logf("kind delete cluster failed: %v\n%s", err, out)
		}
	})
	return h
}

func containsLine(s, want string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func (h *Harness) context() string { return "kind-" + h.ClusterName }

// Run executes a command and fails the test on error.
func (h *Harness) Run(name string, args ...string) {
	h.t.Helper()
	h.t.Logf("+ %s %s", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// Kubectl runs kubectl pinned to this harness's Kind context.
func (h *Harness) Kubectl(args ...string) {
	h.t.Helper()
	h.Run("kubectl", append([]string{"--context", h.context()}, args...)...)
}

// KubectlOutput runs kubectl pinned to this cluster and returns stdout.
// Errors are returned, not fatal — callers poll with it.
func (h *Harness) KubectlOutput(args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", h.context()}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

// DockerBuild builds an image from the repo-root Dockerfile path.
func (h *Harness) DockerBuild(tag, dockerfile, contextDir string) {
	h.t.Helper()
	h.Run("docker", "build", "-t", tag, "-f", dockerfile, contextDir)
}

// KindLoad loads a local docker image into the cluster's nodes.
func (h *Harness) KindLoad(tag string) {
	h.t.Helper()
	h.Run("kind", "load", "docker-image", tag, "--name", h.ClusterName)
}

// PullAndLoad pulls a remote image on the host and loads it into the
// cluster, so pod startup never depends on in-cluster registry access.
func (h *Harness) PullAndLoad(image string) {
	h.t.Helper()
	h.Run("docker", "pull", image)
	h.KindLoad(image)
}

// WaitRollout blocks until the workload's rollout completes.
// kind: "daemonset" or "deployment".
func (h *Harness) WaitRollout(kind, name, namespace string, timeout time.Duration) {
	h.t.Helper()
	h.Kubectl("rollout", "status", kind+"/"+name, "-n", namespace, "--timeout="+timeout.String())
}

// PortForward starts `kubectl port-forward` to the target and returns
// once the tunnel is listening. The forward is torn down via t.Cleanup.
// target is e.g. "ds/ollie-agent"; returns the local base URL.
func (h *Harness) PortForward(target, namespace string, localPort, remotePort int) string {
	h.t.Helper()
	cmd := exec.Command("kubectl", "--context", h.context(),
		"port-forward", "-n", namespace, target,
		fmt.Sprintf("%d:%d", localPort, remotePort))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.t.Fatalf("port-forward stdout pipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("starting port-forward: %v", err)
	}
	h.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			h.t.Logf("port-forward: %s", line)
			if strings.Contains(line, "Forwarding from") {
				ready <- nil
				// Keep draining so the child never blocks on a full pipe.
				for scanner.Scan() {
				}
				return
			}
		}
		ready <- fmt.Errorf("port-forward exited before forwarding: %v", scanner.Err())
	}()
	select {
	case err := <-ready:
		if err != nil {
			h.t.Fatalf("port-forward to %s: %v", target, err)
		}
	case <-time.After(30 * time.Second):
		h.t.Fatalf("port-forward to %s: timed out waiting for tunnel", target)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", localPort)
}

// PollHTTP polls url until predicate returns true or the timeout
// expires; on timeout it fails the test with the last response body.
func (h *Harness) PollHTTP(url string, timeout time.Duration, desc string, predicate func(body string) bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			b, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil {
				last = string(b)
				if predicate(last) {
					return
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	h.saveArtifact("last-poll-response.txt", last)
	h.t.Fatalf("timed out after %s waiting for %s at %s (last response saved to artifacts, %d bytes)",
		timeout, desc, url, len(last))
}

// DumpDiagnostics captures cluster state and component logs. Call from
// a Cleanup so failures leave artifacts behind; CI uploads $ARTIFACTS.
func (h *Harness) DumpDiagnostics() {
	pods, _ := h.KubectlOutput("get", "pods", "-A", "-o", "wide")
	h.saveArtifact("pods.txt", pods)
	events, _ := h.KubectlOutput("get", "events", "-A", "--sort-by=.lastTimestamp")
	h.saveArtifact("events.txt", events)
	for name, args := range map[string][]string{
		"agent.log":      {"logs", "-n", "ollie-system", "ds/ollie-agent", "-c", "agent", "--tail=400"},
		"obi.log":        {"logs", "-n", "ollie-system", "ds/ollie-agent", "-c", "obi", "--tail=400"},
		"controller.log": {"logs", "-n", "ollie-system", "deploy/ollie-controller", "--tail=200"},
	} {
		out, err := h.KubectlOutput(args...)
		if err != nil {
			out += fmt.Sprintf("\n(kubectl error: %v)", err)
		}
		h.saveArtifact(name, out)
	}
}

// saveArtifact writes content under $ARTIFACTS (the path CI uploads) or
// logs a truncated copy when running locally without it.
func (h *Harness) saveArtifact(name, content string) {
	if dir := os.Getenv("ARTIFACTS"); dir != "" {
		path := filepath.Join(dir, "e2e", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			if err := os.WriteFile(path, []byte(content), 0o644); err == nil {
				h.t.Logf("artifact saved: %s", path)
				return
			}
		}
	}
	const cap = 4000
	if len(content) > cap {
		content = content[len(content)-cap:]
	}
	h.t.Logf("--- %s ---\n%s", name, content)
}
