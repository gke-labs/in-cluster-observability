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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// obiImageRe extracts the pinned OBI image tag from k8s/daemonset.yaml
// so the OBI bump PR (#152) doesn't have to touch this test.
var obiImageRe = regexp.MustCompile(`image:\s*(otel/ebpf-instrument:\S+)`)

// TestCaptureSmokeHTTP is the minimal end-to-end gate from ADR-0023 /
// issue #150: build the real images, install k8s/ unmodified via
// kustomize, run real HTTP traffic through an instrumented workload,
// and assert that OBI-derived, K8s-attributed series reach the agent's
// :9090 scrape endpoint. This is the only automated coverage of the
// OBI sibling-container boundary — the ADR-0021 config-knob bug and
// the L7 caps bug were both invisible to unit tests.
func TestCaptureSmokeHTTP(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set, skipping e2e test")
	}

	repoRoot := gitRoot(t)
	obiImage := pinnedOBIImage(t, repoRoot)

	h := NewHarness(t, "ollie-e2e")
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	// Build our images; preload everything the pods need so the
	// cluster never pulls from a registry (CI-friendly: no Docker Hub
	// rate-limit exposure inside the nodes).
	h.DockerBuild("ollie:e2e", filepath.Join(repoRoot, "images/ollie/Dockerfile"), repoRoot)
	h.KindLoad("ollie:e2e")
	h.DockerBuild("ollie-controller:e2e", filepath.Join(repoRoot, "images/ollie-controller/Dockerfile"), repoRoot)
	h.KindLoad("ollie-controller:e2e")
	h.PullAndLoad(obiImage)

	// Install exactly what a user installs.
	h.Kubectl("apply", "-k", filepath.Join(repoRoot, "k8s"))

	// The manifests use bare image names (ap deploy fills the registry
	// at deploy time; see AGENTS.md). Point them at the just-loaded
	// e2e tags. Strategic-merge by container name.
	h.Kubectl("patch", "daemonset", "ollie-agent", "-n", "ollie-system", "--type=strategic",
		"-p", `{"spec":{"template":{"spec":{"containers":[{"name":"agent","image":"ollie:e2e","imagePullPolicy":"Never"}]}}}}`)
	h.Kubectl("patch", "deployment", "ollie-controller", "-n", "ollie-system", "--type=strategic",
		"-p", `{"spec":{"template":{"spec":{"containers":[{"name":"controller","image":"ollie-controller:e2e","imagePullPolicy":"Never"}]}}}}`)

	h.WaitRollout("daemonset", "ollie-agent", "ollie-system", 5*time.Minute)
	h.WaitRollout("deployment", "ollie-controller", "ollie-system", 3*time.Minute)

	// Test workload + traffic. agnhost (registry.k8s.io — no Docker
	// Hub) serves HTTP on 8080, which is in the DaemonSet's
	// --obi-instrument-ports seed list, so OBI's discovery attaches
	// its HTTP/1.1 uprobes to it. The client loops wget through the
	// Service so requests cross the pod network.
	const agnhost = "registry.k8s.io/e2e-test-images/agnhost:2.53"
	h.PullAndLoad(agnhost)
	applyStdin(t, h, testWorkloadManifest(agnhost))
	h.WaitRollout("deployment", "echo", "default", 2*time.Minute)
	h.WaitRollout("deployment", "traffic-client", "default", 2*time.Minute)

	// Assert on the agent's production scrape surface. port-forward
	// terminates on the pod's loopback, which --scrape-auth exempts by
	// design (#145), so no token is needed here; in-cluster scrapes
	// still require one.
	base := h.PortForward("ds/ollie-agent", "ollie-system", 19090, 9090)

	// Agent self-observability must be present immediately.
	h.PollHTTP(base+"/metrics", 1*time.Minute, "agent self-obs metrics",
		func(body string) bool {
			return strings.Contains(body, "ollie_")
		})

	// The real assertion: an OBI-translated HTTP server metric carrying
	// the echo pod's K8s identity, attached by OBI's own informer
	// (ADR-0021). Accept any Prometheus rendering of
	// http.server.request.duration (suffix variants differ across
	// exporter versions).
	h.PollHTTP(base+"/metrics", 3*time.Minute, "OBI HTTP series with k8s identity for the echo pod",
		func(body string) bool {
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "#") {
					continue
				}
				if strings.Contains(line, "http_server_request_duration") &&
					strings.Contains(line, `k8s_pod_name="echo-`) {
					return true
				}
			}
			return false
		})
}

func gitRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("finding repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func pinnedOBIImage(t *testing.T, repoRoot string) string {
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

// applyStdin feeds a manifest to kubectl apply -f - on the harness's
// pinned context.
func applyStdin(t *testing.T, h *Harness, manifest string) {
	t.Helper()
	cmd := exec.Command("kubectl", "--context", "kind-"+h.ClusterName, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply workload manifest: %v\n%s", err, out)
	}
}

func testWorkloadManifest(agnhostImage string) string {
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
`, agnhostImage)
}
