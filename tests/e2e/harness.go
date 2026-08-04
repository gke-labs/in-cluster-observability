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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
	if err == nil && containsLine(string(out), clusterName) && h.clusterAlive() && h.nodeCount() == clusterNodes {
		t.Logf("kind cluster %q already exists; reusing", clusterName)
	} else {
		// A cluster that is listed but not answering is usually one
		// caught mid-teardown by the previous test's cleanup (kind
		// delete can return while the node container is still dying);
		// exec'ing into it fails with setns errors. Delete + recreate.
		// A cluster with the wrong node count is a leftover from
		// before the multi-node profile (#194); recreate it too.
		if err == nil && containsLine(string(out), clusterName) {
			t.Logf("kind cluster %q exists but is not usable (dead or wrong topology); recreating", clusterName)
			_ = exec.Command("kind", "delete", "cluster", "--name", clusterName).Run()
		}
		t.Logf("creating kind cluster %q (%d nodes)", clusterName, clusterNodes)
		cfg := filepath.Join(t.TempDir(), "kind.yaml")
		if err := os.WriteFile(cfg, []byte(kindConfig), 0o600); err != nil {
			t.Fatalf("write kind config: %v", err)
		}
		h.Run("kind", "create", "cluster", "--name", clusterName, "--config", cfg, "--wait", "2m")
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

// The shared cluster is MULTI-NODE (#194): one control-plane + one
// worker, so cross-node behavior — per-node series staying distinct
// under the fan-out merge, degraded semantics when one node's agent
// dies — is exercised against real topology instead of only
// in-process simulations. The v0.5 cross-node merge bug shipped with
// green CI precisely because the old cluster was single-node. Two
// nodes keeps runner cost bounded; the agent DaemonSet tolerates the
// control-plane taint (`operator: Exists`), so both nodes run agents.
const clusterNodes = 2

const kindConfig = `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
`

// nodeCount returns the number of Ready-or-not nodes the shared
// cluster reports, or 0 when unreachable.
func (h *Harness) nodeCount() int {
	out, err := exec.Command("kubectl", "--context", "kind-"+h.ClusterName,
		"get", "nodes", "-o", "name").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

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

// probeImage is the in-cluster auth-probe image (tests/e2e/probe),
// loaded once per run by BuildProbeImage.
const probeImage = "ollie-e2e-probe:e2e"

// probeState makes BuildProbeImage once-per-run, mirroring installState.
var probeState struct {
	sync.Mutex
	built  bool
	failed string
}

// BuildProbeImage compiles the stdlib probe (tests/e2e/probe) into a
// FROM-scratch image and loads it into the shared cluster, once per run.
// The probe is the only way to hit the agent/query auth boundaries from
// a real pod IP: port-forward terminates on loopback, which every auth
// layer exempts by design (#145), so a forwarded port can never observe
// a rejection.
func (h *Harness) BuildProbeImage(repoRoot string) {
	h.t.Helper()
	probeState.Lock()
	defer probeState.Unlock()
	if probeState.failed != "" {
		h.t.Fatalf("skipping: earlier probe build failed: %s", probeState.failed)
	}
	if probeState.built {
		return
	}
	probeState.failed = "BuildProbeImage did not complete"

	dir := h.t.TempDir()
	bin := filepath.Join(dir, "probe")
	build := exec.Command("go", "build", "-o", bin, "./tests/e2e/probe")
	build.Dir = repoRoot
	// Static linux binary for a scratch image; match the kind node arch
	// (== host arch).
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		h.t.Fatalf("building probe: %v\n%s", err, out)
	}
	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dockerfile,
		[]byte("FROM scratch\nCOPY probe /probe\nENTRYPOINT [\"/probe\"]\n"), 0o600); err != nil {
		h.t.Fatalf("write probe Dockerfile: %v", err)
	}
	h.DockerBuild(probeImage, dockerfile, dir)
	h.KindLoad(probeImage)

	probeState.built = true
	probeState.failed = ""
}

// EnsureNamespace creates the namespace if it does not already exist.
// The API server stamps every namespace with the immutable
// kubernetes.io/metadata.name label, so a namespace named to match a
// NetworkPolicy's namespaceSelector is admitted by that policy — this
// is how the scrape probe lands on the agent's permitted-scraper side.
func (h *Harness) EnsureNamespace(name string) {
	h.t.Helper()
	if _, err := h.KubectlOutput("get", "namespace", name); err == nil {
		return
	}
	h.Kubectl("create", "namespace", name)
}

// RunProbe runs the probe image as a one-off pod in the given namespace
// with an untrusted, non-ollie ServiceAccount, passing the probe args,
// waits for it to finish, and returns its stdout. The pod is deleted on
// cleanup. BuildProbeImage must have run first.
func (h *Harness) RunProbe(namespace, podName string, args ...string) string {
	h.t.Helper()
	_, _ = h.KubectlOutput("delete", "pod", podName, "-n", namespace, "--ignore-not-found", "--now")
	runArgs := []string{"run", podName, "-n", namespace,
		"--image=" + probeImage, "--image-pull-policy=Never",
		"--restart=Never", "--command", "--", "/probe"}
	runArgs = append(runArgs, args...)
	h.Kubectl(runArgs...)
	h.t.Cleanup(func() {
		_, _ = h.KubectlOutput("delete", "pod", podName, "-n", namespace, "--ignore-not-found", "--now")
	})
	h.PollKubectl(2*time.Minute, "probe pod "+podName+" reaches a terminal phase",
		func() (string, error) {
			return h.KubectlOutput("get", "pod", podName, "-n", namespace,
				"-o", "jsonpath={.status.phase}")
		},
		func(out string) bool {
			p := strings.TrimSpace(out)
			return p == "Succeeded" || p == "Failed"
		})
	out, err := h.KubectlOutput("logs", podName, "-n", namespace)
	if err != nil {
		h.t.Fatalf("probe %s logs: %v", podName, err)
	}
	return out
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

// PortForwardTLS is PortForward with an https base URL, for the query
// server's :9095 which serves TLS (ADR-0029). PollHTTP's client skips
// certificate verification — the strict chain-of-trust assertions live
// in TestIntraTLSVerification; here the tunnel already runs through
// the authenticated API server connection.
func (h *Harness) PortForwardTLS(target, namespace string, localPort, remotePort int) string {
	h.t.Helper()
	return "https" + strings.TrimPrefix(h.PortForward(target, namespace, localPort, remotePort), "http")
}

// insecureClient tolerates the self-signed/CA-issued serving certs on
// intra-ollie listeners when polling through port-forward tunnels.
var insecureClient = &http.Client{Transport: &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test tunnel; verification covered by TestIntraTLSVerification
}}

// PollHTTP polls url until predicate returns true or the timeout
// expires; on timeout it fails the test with the last response body.
func (h *Harness) PollHTTP(url string, timeout time.Duration, desc string, predicate func(body string) bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := insecureClient.Get(url)
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

// PollHTTPUntil is the non-fatal form of PollHTTP: it returns true if
// predicate matches before the timeout and false otherwise, without
// failing the test. Callers use it to detect a dead port-forward tunnel
// (e.g. one bound to a pod that got replaced) and re-establish it.
func (h *Harness) PollHTTPUntil(url string, timeout time.Duration, predicate func(body string) bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := insecureClient.Get(url)
		if err == nil {
			b, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil && predicate(string(b)) {
				return true
			}
		}
		time.Sleep(3 * time.Second)
	}
	return false
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

// tlsServerImage is the stdlib Go crypto/tls echo server + client
// (tests/e2e/tlsserver), loaded once per run for the TLS-decrypt tests.
const tlsServerImage = "ollie-e2e-tlsserver:e2e"

// nginxImage is a stock, Debian-based nginx that dynamically links
// system OpenSSL (libssl.so) — the uprobe target OBI needs for the
// OpenSSL TLS-decrypt test (#107). Pinned like the OBI/collector images;
// bump deliberately.
const nginxImage = "nginx:1.27"

// tlsServerState guards the tlsserver image build once-per-run; the two
// deploy guards below mirror installState.deployed so each TLS workload
// is stood up at most once in the shared cluster.
var tlsServerState struct {
	sync.Mutex
	built  bool
	failed string
}

var tlsGoState struct {
	sync.Mutex
	deployed bool
}

var tlsNginxState struct {
	sync.Mutex
	deployed bool
}

// BuildTLSServerImage compiles the stdlib tlsserver (tests/e2e/tlsserver)
// into a FROM-scratch image and loads it into the shared cluster, once
// per run. The binary is intentionally NOT stripped: OBI's Go-TLS
// uprobes resolve crypto/tls functions from the Go symbol table, so a
// `-ldflags=-s -w` strip would blind the very decrypt path this tests.
func (h *Harness) BuildTLSServerImage(repoRoot string) {
	h.t.Helper()
	tlsServerState.Lock()
	defer tlsServerState.Unlock()
	if tlsServerState.failed != "" {
		h.t.Fatalf("skipping: earlier tlsserver build failed: %s", tlsServerState.failed)
	}
	if tlsServerState.built {
		return
	}
	tlsServerState.failed = "BuildTLSServerImage did not complete"

	dir := h.t.TempDir()
	bin := filepath.Join(dir, "tlsserver")
	build := exec.Command("go", "build", "-o", bin, "./tests/e2e/tlsserver")
	build.Dir = repoRoot
	// Static linux binary for a scratch image; match the kind node arch
	// (== host arch).
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		h.t.Fatalf("building tlsserver: %v\n%s", err, out)
	}
	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dockerfile,
		[]byte("FROM scratch\nCOPY tlsserver /tlsserver\nENTRYPOINT [\"/tlsserver\"]\n"), 0o600); err != nil {
		h.t.Fatalf("write tlsserver Dockerfile: %v", err)
	}
	h.DockerBuild(tlsServerImage, dockerfile, dir)
	h.KindLoad(tlsServerImage)

	tlsServerState.built = true
	tlsServerState.failed = ""
}

// DeployTLSGoWorkload starts the Go crypto/tls HTTPS echo server (#106)
// plus a client that loops HTTPS requests through the Service, so the
// encrypted traffic crosses the pod network where OBI's Go-TLS uprobes
// attach. BuildTLSServerImage must have run first.
func (h *Harness) DeployTLSGoWorkload() {
	h.t.Helper()
	tlsGoState.Lock()
	defer tlsGoState.Unlock()
	if tlsGoState.deployed {
		h.t.Log("tls-go workload already deployed in the shared cluster; reusing")
		return
	}
	h.ApplyStdin(tlsGoWorkloadManifest())
	h.WaitRollout("deployment", "tls-go", "default", 2*time.Minute)
	h.WaitRollout("deployment", "tls-go-client", "default", 2*time.Minute)
	tlsGoState.deployed = true
}

// tlsGoWorkloadManifest is the Go crypto/tls server + its HTTPS client
// loop. Server listens on 8443 (in the --obi-instrument-ports seed);
// the client dials the Service by DNS so traffic crosses the pod
// network, skipping cert verification (the server is self-signed).
func tlsGoWorkloadManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: tls-go
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: tls-go}
  template:
    metadata:
      labels: {app: tls-go}
    spec:
      containers:
        - name: server
          image: %[1]s
          imagePullPolicy: Never
          args: ["serve"]
          ports: [{containerPort: 8443}]
---
apiVersion: v1
kind: Service
metadata:
  name: tls-go
  namespace: default
spec:
  selector: {app: tls-go}
  ports: [{port: 8443, targetPort: 8443}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tls-go-client
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: tls-go-client}
  template:
    metadata:
      labels: {app: tls-go-client}
    spec:
      containers:
        - name: client
          image: %[1]s
          imagePullPolicy: Never
          args: ["client", "https://tls-go.default.svc:8443/", "200"]
`, tlsServerImage)
}

// nginxTLSConf is a minimal self-contained nginx config serving HTTPS on
// 8443 from a mounted cert. Mounted over /etc/nginx/nginx.conf via
// subPath so the stock image's mime.types / conf.d stay intact.
const nginxTLSConf = `events {}
http {
  access_log off;
  server {
    listen 8443 ssl;
    ssl_certificate     /etc/nginx/tls/tls.crt;
    ssl_certificate_key /etc/nginx/tls/tls.key;
    location / { return 200 "ok\n"; }
  }
}
`

// DeployTLSOpenSSLWorkload stands up a stock nginx (dynamically linked
// against system OpenSSL) serving HTTPS on 8443, plus the Go client
// looping HTTPS requests at it (#107). The cert is self-signed and
// created as a Secret; nginx.conf as a ConfigMap. The point is to give
// OBI's OpenSSL (libssl.so) uprobes real encrypted server-side traffic
// to decrypt — the http.server series then proves the decrypt worked.
func (h *Harness) DeployTLSOpenSSLWorkload() {
	h.t.Helper()
	tlsNginxState.Lock()
	defer tlsNginxState.Unlock()
	if tlsNginxState.deployed {
		h.t.Log("tls-nginx workload already deployed in the shared cluster; reusing")
		return
	}
	h.PullAndLoad(nginxImage)

	// Self-signed serving cert for nginx. The client skips verification,
	// so this only needs to be well-formed, not chained to a trusted CA.
	certPEM, keyPEM := h.genSelfSignedCertPEM("tls-nginx.default.svc", "tls-nginx", "localhost")
	dir := h.t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	confFile := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		h.t.Fatalf("write nginx cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		h.t.Fatalf("write nginx key: %v", err)
	}
	if err := os.WriteFile(confFile, []byte(nginxTLSConf), 0o600); err != nil {
		h.t.Fatalf("write nginx conf: %v", err)
	}
	// create (not apply): the once-per-run guard means these never
	// pre-exist, and `create secret tls` avoids hand-base64'ing PEM.
	h.Kubectl("create", "secret", "tls", "tls-nginx-cert", "-n", "default",
		"--cert="+certFile, "--key="+keyFile)
	h.Kubectl("create", "configmap", "tls-nginx-conf", "-n", "default",
		"--from-file=nginx.conf="+confFile)

	h.ApplyStdin(tlsNginxWorkloadManifest())
	h.WaitRollout("deployment", "tls-nginx", "default", 2*time.Minute)
	h.WaitRollout("deployment", "tls-nginx-client", "default", 2*time.Minute)
	tlsNginxState.deployed = true
}

// tlsNginxWorkloadManifest is the nginx HTTPS server (cert Secret +
// conf ConfigMap mounted) and the Go client loop pointed at it.
func tlsNginxWorkloadManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: tls-nginx
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: tls-nginx}
  template:
    metadata:
      labels: {app: tls-nginx}
    spec:
      containers:
        - name: nginx
          image: %[1]s
          imagePullPolicy: Never
          ports: [{containerPort: 8443}]
          volumeMounts:
            - name: conf
              mountPath: /etc/nginx/nginx.conf
              subPath: nginx.conf
            - name: cert
              mountPath: /etc/nginx/tls
              readOnly: true
      volumes:
        - name: conf
          configMap: {name: tls-nginx-conf}
        - name: cert
          secret: {secretName: tls-nginx-cert}
---
apiVersion: v1
kind: Service
metadata:
  name: tls-nginx
  namespace: default
spec:
  selector: {app: tls-nginx}
  ports: [{port: 8443, targetPort: 8443}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tls-nginx-client
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: tls-nginx-client}
  template:
    metadata:
      labels: {app: tls-nginx-client}
    spec:
      containers:
        - name: client
          image: %[2]s
          imagePullPolicy: Never
          args: ["client", "https://tls-nginx.default.svc:8443/", "200"]
`, nginxImage, tlsServerImage)
}

// genSelfSignedCertPEM mints an ECDSA P-256 self-signed serving cert and
// returns it as PEM cert + key. Used for the nginx workload's Secret.
func (h *Harness) genSelfSignedCertPEM(dnsNames ...string) (certPEM, keyPEM []byte) {
	h.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		h.t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		h.t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		h.t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
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
