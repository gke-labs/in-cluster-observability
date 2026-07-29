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
	"regexp"
	"strings"
	"sync"
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
	if err == nil && containsLine(string(out), clusterName) && h.clusterAlive() {
		t.Logf("kind cluster %q already exists; reusing", clusterName)
	} else {
		// A cluster that is listed but not answering is usually one
		// caught mid-teardown by the previous test's cleanup (kind
		// delete can return while the node container is still dying);
		// exec'ing into it fails with setns errors. Delete + recreate.
		if err == nil && containsLine(string(out), clusterName) {
			t.Logf("kind cluster %q exists but is not responding; recreating", clusterName)
			_ = exec.Command("kind", "delete", "cluster", "--name", clusterName).Run()
		}
		t.Logf("creating kind cluster %q", clusterName)
		h.Run("kind", "create", "cluster", "--name", clusterName, "--wait", "2m")
	}

	// Teardown is owned by TestMain: the cluster (and the install
	// inside it) is shared across the whole suite run so the 6-test
	// e2e fits go test's 20m budget — per-test create+install cost
	// ~3-4 min each and blew it. KEEP_CLUSTER still leaves it up.
	return h
}

// TestMain deletes the shared cluster once the whole suite is done.
func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv("RUN_E2E") != "" && os.Getenv("KEEP_CLUSTER") == "" {
		_ = exec.Command("kind", "delete", "cluster", "--name", sharedClusterName).Run()
	}
	os.Exit(code)
}

// sharedClusterName is the one cluster every e2e test shares.
const sharedClusterName = "ollie-e2e"

// installState makes InstallOllie / DeployTestWorkload once-per-run:
// later tests reuse the deployed stack instead of re-building and
// re-rolling it. A failed install fails every subsequent test fast.
var installState struct {
	sync.Mutex
	installed bool
	deployed  bool
	failed    string
}

// clusterAlive reports whether the named cluster's API server
// answers and its node container accepts exec (the two things a
// half-deleted cluster fails).
func (h *Harness) clusterAlive() bool {
	if err := exec.Command("kubectl", "--context", h.context(),
		"get", "nodes", "--request-timeout=15s").Run(); err != nil {
		return false
	}
	return exec.Command("docker", "exec", h.ClusterName+"-control-plane", "true").Run() == nil
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

// KubectlRetry runs kubectl, retrying transient failures (e.g. the
// aggregator 500-window right after an APIService lands) before
// failing the test.
func (h *Harness) KubectlRetry(attempts int, args ...string) {
	h.t.Helper()
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(5 * time.Second)
		}
		full := append([]string{"--context", h.context()}, args...)
		h.t.Logf("+ kubectl %s (attempt %d/%d)", strings.Join(args, " "), i+1, attempts)
		cmd := exec.Command("kubectl", full...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return
		}
		lastErr = fmt.Errorf("kubectl %v: %w: %s", args, err, out)
	}
	h.t.Fatal(lastErr)
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

// PollKubectl polls fn until predicate accepts its output or the
// timeout expires; on timeout it fails the test with the last output
// (kubectl errors count as output so 404s from --raw are visible).
func (h *Harness) PollKubectl(timeout time.Duration, desc string, fn func() (string, error), predicate func(out string) bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := fn()
		if err != nil {
			last = out + " (error: " + err.Error() + ")"
		} else {
			last = out
			if predicate(out) {
				return
			}
		}
		time.Sleep(3 * time.Second)
	}
	h.saveArtifact("last-poll-kubectl.txt", last)
	h.t.Fatalf("timed out after %s waiting for %s (last output saved to artifacts): %.500s",
		timeout, desc, last)
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

// ApplyStdin feeds a manifest to kubectl apply -f - on the harness's
// pinned context.
func (h *Harness) ApplyStdin(manifest string) {
	h.t.Helper()
	cmd := exec.Command("kubectl", "--context", h.context(), "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("kubectl apply manifest from stdin: %v\n%s", err, out)
	}
}

// GitRoot returns the repository root.
func GitRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("finding repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// obiImageRe extracts the pinned OBI image tag from k8s/daemonset.yaml
// so tests never hardcode a tag the #152-style bump PRs would have to
// chase.
var obiImageRe = regexp.MustCompile(`image:\s*(otel/ebpf-instrument:\S+)`)

// PinnedOBIImage returns the OBI image pinned in k8s/daemonset.yaml.
func PinnedOBIImage(t *testing.T, repoRoot string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "k8s/daemonset.yaml"))
	if err != nil {
		t.Fatalf("reading daemonset manifest: %v", err)
	}
	m := obiImageRe.FindSubmatch(b)
	if m == nil {
		t.Fatalf("no otel/ebpf-instrument image pin found in k8s/daemonset.yaml")
	}
	return string(m[1])
}

// AgnhostImage is the test workload image. registry.k8s.io, so cluster
// pulls never depend on Docker Hub rate limits.
const AgnhostImage = "registry.k8s.io/e2e-test-images/agnhost:2.53"

// TestWorkloadManifest returns an HTTP echo server on 8080 (a port in
// the DaemonSet's --obi-instrument-ports seed list) plus a client
// deployment that loops requests through the Service so traffic
// crosses the pod network.
func TestWorkloadManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: echo}
  template:
    metadata:
      labels: {app: echo}
    spec:
      containers:
        - name: echo
          image: %[1]s
          imagePullPolicy: Never
          args: ["netexec", "--http-port=8080"]
          ports: [{containerPort: 8080}]
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: default
spec:
  selector: {app: echo}
  ports: [{port: 8080, targetPort: 8080}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: traffic-client
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: traffic-client}
  template:
    metadata:
      labels: {app: traffic-client}
    spec:
      containers:
        - name: client
          image: %[1]s
          imagePullPolicy: Never
          command: ["/bin/sh", "-c"]
          args:
            - while true; do wget -q -O /dev/null http://echo.default.svc:8080/hostname || true; sleep 0.2; done
`, AgnhostImage)
}

// InstallOllie builds + loads the ollie images, installs k8s/ via
// kustomize, points the workloads at the freshly-loaded e2e tags, and
// waits for rollout. Shared by the e2e smoke test and the contract
// fixture recorder.
func (h *Harness) InstallOllie(repoRoot string) {
	h.t.Helper()
	installState.Lock()
	defer installState.Unlock()
	if installState.failed != "" {
		h.t.Fatalf("skipping: earlier install failed: %s", installState.failed)
	}
	if installState.installed {
		h.t.Log("ollie already installed in the shared cluster; reusing")
		return
	}
	installState.failed = "InstallOllie did not complete"
	h.DockerBuild("ollie:e2e", filepath.Join(repoRoot, "images/ollie/Dockerfile"), repoRoot)
	h.KindLoad("ollie:e2e")
	h.DockerBuild("ollie-controller:e2e", filepath.Join(repoRoot, "images/ollie-controller/Dockerfile"), repoRoot)
	h.KindLoad("ollie-controller:e2e")
	h.DockerBuild("ollie-query:e2e", filepath.Join(repoRoot, "images/ollie-query/Dockerfile"), repoRoot)
	h.KindLoad("ollie-query:e2e")
	h.PullAndLoad(PinnedOBIImage(h.t, repoRoot))

	h.Kubectl("apply", "-k", filepath.Join(repoRoot, "k8s"))

	// Applying the custom-metrics APIService (v0.5, #96) makes the
	// kube-apiserver's aggregator re-wire; requests racing that
	// window can catch transient 500s (observed as a failed
	// /readyz probe right after apply). Settle before patching.
	h.PollKubectl(2*time.Minute, "kube-apiserver /readyz after APIService registration",
		func() (string, error) { return h.KubectlOutput("get", "--raw", "/readyz") },
		func(out string) bool { return strings.Contains(out, "ok") })

	h.KubectlRetry(3, "patch", "daemonset", "ollie-agent", "-n", "ollie-system", "--type=strategic",
		"-p", `{"spec":{"template":{"spec":{"containers":[{"name":"agent","image":"ollie:e2e","imagePullPolicy":"Never"}]}}}}`)
	h.KubectlRetry(3, "patch", "deployment", "ollie-controller", "-n", "ollie-system", "--type=strategic",
		"-p", `{"spec":{"template":{"spec":{"containers":[{"name":"controller","image":"ollie-controller:e2e","imagePullPolicy":"Never"}]}}}}`)
	h.KubectlRetry(3, "patch", "deployment", "ollie-query", "-n", "ollie-system", "--type=strategic",
		"-p", `{"spec":{"template":{"spec":{"containers":[{"name":"query","image":"ollie-query:e2e","imagePullPolicy":"Never"}]}}}}`)
	h.WaitRollout("daemonset", "ollie-agent", "ollie-system", 5*time.Minute)
	h.WaitRollout("deployment", "ollie-controller", "ollie-system", 3*time.Minute)
	h.WaitRollout("deployment", "ollie-query", "ollie-system", 3*time.Minute)
	installState.installed = true
	installState.failed = ""
}

// DeployTestWorkload loads the agnhost image and starts the echo
// server + traffic client.
func (h *Harness) DeployTestWorkload() {
	h.t.Helper()
	installState.Lock()
	defer installState.Unlock()
	if installState.deployed {
		h.t.Log("test workload already deployed in the shared cluster; reusing")
		return
	}
	h.PullAndLoad(AgnhostImage)
	h.ApplyStdin(TestWorkloadManifest())
	h.WaitRollout("deployment", "echo", "default", 2*time.Minute)
	h.WaitRollout("deployment", "traffic-client", "default", 2*time.Minute)
	installState.deployed = true
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

// collectorImage is the OTel collector used by the export e2e test.
// Pinned like the OBI image; bump deliberately.
const collectorImage = "otel/opentelemetry-collector:0.111.0"
