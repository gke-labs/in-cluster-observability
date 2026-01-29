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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservabilityAgent(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("E2E env var not set, skipping")
	}

	h := NewHarness(t, "observability-agent-e2e")
	h.Setup()

	gitRoot := h.GetGitRoot()

	// Build and load image
	h.DockerBuild("in-cluster-observability-agent:e2e", filepath.Join(gitRoot, "images/in-cluster-observability-agent/Dockerfile"), gitRoot)
	h.KindLoad("in-cluster-observability-agent:e2e")

	// Deploy Agent from manifests
	h.KubectlApplyFile(filepath.Join(gitRoot, "k8s/manifests.yaml"))

	// Patch DaemonSet to use the e2e image we just built
	h.RunCommand("kubectl", "set", "image", "daemonset/in-cluster-observability-agent", "in-cluster-observability-agent=in-cluster-observability-agent:e2e")
	// Set imagePullPolicy to Never so it uses the loaded image
	h.RunCommand("kubectl", "patch", "daemonset", "in-cluster-observability-agent", "--type=json", "-p", `[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]`)

	h.WaitForDaemonSet("in-cluster-observability-agent", 2*time.Minute)

	// Wait for pods to be ready
	h.WaitForPodReady("app=in-cluster-observability-agent", 1*time.Minute)

	// Run a pod to curl the metrics
	clientPodName := "test-client"
	h.DeletePod(clientPodName)

	clientManifest := `
apiVersion: v1
kind: Pod
metadata:
  name: test-client
spec:
  containers:
  - name: curl
    image: curlimages/curl:latest
    command: ["curl", "-s", "http://in-cluster-observability-agent.default.svc.cluster.local:8080/metrics"]
  restartPolicy: Never
`
	h.KubectlApplyContent(clientManifest)

	h.WaitForPodSuccess(clientPodName, 1*time.Minute)

	logs := h.GetPodLogs(clientPodName)
	t.Logf("Client logs: %s", logs)

	// Verify metrics are present
	if !strings.Contains(logs, "node_network_receive_bytes_total") {
		t.Error("Metrics do not contain node_network_receive_bytes_total")
	}
	if !strings.Contains(logs, "node_network_transmit_bytes_total") {
		t.Error("Metrics do not contain node_network_transmit_bytes_total")
	}
}
