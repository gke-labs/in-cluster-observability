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
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type Harness struct {
	ClusterName string
	t           *testing.T
}

func NewHarness(t *testing.T, clusterName string) *Harness {
	return &Harness{
		ClusterName: clusterName,
		t:           t,
	}
}

func (h *Harness) Setup() {
	h.t.Helper()
	// Check if cluster exists
	cmd := exec.Command("kind", "get", "clusters")
	out, err := cmd.Output()
	if err == nil && strings.Contains(string(out), h.ClusterName) {
		h.t.Logf("Cluster %s already exists", h.ClusterName)
		return
	}

	h.t.Logf("Creating cluster %s", h.ClusterName)
	cmd = exec.Command("kind", "create", "cluster", "--name", h.ClusterName)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("Failed to create cluster: %v\nOutput: %s", err, out)
	}

	h.t.Cleanup(func() {
		h.CollectLogs()
		h.Teardown()
	})
}

func (h *Harness) Teardown() {
	h.t.Helper()
	h.t.Logf("Deleting cluster %s", h.ClusterName)
	cmd := exec.Command("kind", "delete", "cluster", "--name", h.ClusterName)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Logf("Failed to delete cluster: %v\nOutput: %s", err, out)
	}
}

func (h *Harness) GetGitRoot() string {
	h.t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("Failed to find git root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func (h *Harness) RunCommand(name string, args ...string) {
	h.t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("Command failed: %s %v\nOutput: %s", name, args, out)
	}
}

func (h *Harness) DockerBuild(tag, dockerfile, context string) {
	h.t.Helper()
	h.t.Logf("Building docker image %s", tag)
	h.RunCommand("docker", "build", "-t", tag, "-f", dockerfile, context)
}

func (h *Harness) KindLoad(tag string) {
	h.t.Helper()
	h.t.Logf("Loading image %s into kind", tag)
	h.RunCommand("kind", "load", "docker-image", tag, "--name", h.ClusterName)
}

func (h *Harness) KubectlApplyContent(content string) {
	h.t.Helper()
	h.t.Logf("Applying manifest content")
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewBufferString(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("Failed to apply content: %v\nOutput: %s", err, out)
	}
}

func (h *Harness) KubectlApplyFile(path string) {
	h.t.Helper()
	h.t.Logf("Applying manifest file %s", path)
	h.RunCommand("kubectl", "apply", "-f", path)
}

func (h *Harness) WaitForDeployment(name string, namespace string, timeout time.Duration) {
	h.t.Helper()
	h.t.Logf("Waiting for deployment %s in namespace %s", name, namespace)
	h.RunCommand("kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout="+timeout.String())
}

func (h *Harness) WaitForDaemonSet(name string, namespace string, timeout time.Duration) {
	h.t.Helper()
	h.t.Logf("Waiting for DaemonSet %s in namespace %s", name, namespace)
	h.RunCommand("kubectl", "rollout", "status", "daemonset/"+name, "-n", namespace, "--timeout="+timeout.String())
}

func (h *Harness) WaitForPodReady(labelSelector string, namespace string, timeout time.Duration) {
	h.t.Helper()
	h.t.Logf("Waiting for pod with label %s to be ready in namespace %s", labelSelector, namespace)
	h.RunCommand("kubectl", "wait", "--for=condition=ready", "pod", "-l", labelSelector, "-n", namespace, "--timeout="+timeout.String())
}

func (h *Harness) DeletePod(name string, namespace string) {
	h.t.Helper()
	// Ignore errors if pod doesn't exist
	exec.Command("kubectl", "delete", "pod", name, "-n", namespace, "--ignore-not-found").Run()
}

func (h *Harness) GetPodLogs(name string, namespace string) string {
	h.t.Helper()
	out, err := exec.Command("kubectl", "logs", name, "-n", namespace).CombinedOutput()
	if err != nil {
		h.t.Fatalf("Failed to get logs for pod %s: %v", name, err)
	}
	return string(out)
}

func (h *Harness) WaitForReplicas(deploymentName string, namespace string, expectedReplicas string, timeout time.Duration) {
	h.t.Helper()
	h.t.Logf("Waiting for deployment %s to have %s replicas", deploymentName, expectedReplicas)
	start := time.Now()
	for {
		if time.Since(start) > timeout {
			h.t.Fatalf("Timed out waiting for deployment %s to have %s replicas", deploymentName, expectedReplicas)
		}
		cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", namespace, "-o", "jsonpath={.status.readyReplicas}")
		out, err := cmd.Output()
		if err == nil && string(out) == expectedReplicas {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func (h *Harness) CollectLogs() {
	h.t.Helper()
	h.t.Log("Collecting E2E test logs and artifacts...")

	runCmd := func(name string, args ...string) string {
		cmd := exec.Command(name, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Sprintf("Command failed: %v\nOutput: %s", err, string(out))
		}
		return string(out)
	}

	// 1. Collect cluster info and resources status
	h.saveArtifact("pods.txt", runCmd("kubectl", "get", "pods", "-A", "-o", "wide"))
	h.saveArtifact("services.txt", runCmd("kubectl", "get", "svc", "-A", "-o", "wide"))
	h.saveArtifact("deployments.txt", runCmd("kubectl", "get", "deploy", "-A", "-o", "wide"))
	h.saveArtifact("daemonsets.txt", runCmd("kubectl", "get", "ds", "-A", "-o", "wide"))
	h.saveArtifact("hpa.txt", runCmd("kubectl", "get", "hpa", "-A"))
	h.saveArtifact("events.txt", runCmd("kubectl", "get", "events", "-A", "--sort-by=.metadata.creationTimestamp"))
	h.saveArtifact("apiservices.txt", runCmd("kubectl", "get", "apiservice"))

	// 2. Describe critical workloads
	h.saveArtifact("describe-test-app.txt", runCmd("kubectl", "describe", "deploy/test-app", "-n", "default"))
	h.saveArtifact("describe-test-client.txt", runCmd("kubectl", "describe", "deploy/test-client", "-n", "default"))
	h.saveArtifact("describe-query-server.txt", runCmd("kubectl", "describe", "deploy/query-server", "-n", "observability-system"))
	h.saveArtifact("describe-hpa.txt", runCmd("kubectl", "describe", "hpa/test-app", "-n", "default"))

	// 3. Dump logs of all pods in relevant namespaces
	namespaces := []string{"observability-system", "default", "cert-manager"}
	for _, ns := range namespaces {
		podsJSON := runCmd("kubectl", "get", "pods", "-n", ns, "-o", "jsonpath={range .items[*]}{.metadata.name}{'\\n'}{end}")
		podNames := strings.Split(strings.TrimSpace(podsJSON), "\n")
		for _, pod := range podNames {
			pod = strings.TrimSpace(pod)
			if pod == "" {
				continue
			}
			// Get container names for the pod
			containersJSON := runCmd("kubectl", "get", "pod", pod, "-n", ns, "-o", "jsonpath={range .spec.containers[*]}{.name}{'\\n'}{end}")
			containers := strings.Split(strings.TrimSpace(containersJSON), "\n")
			for _, container := range containers {
				container = strings.TrimSpace(container)
				if container == "" {
					continue
				}
				logs := runCmd("kubectl", "logs", pod, "-n", ns, "-c", container)
				h.saveArtifact(fmt.Sprintf("logs/%s/%s-%s.log", ns, pod, container), logs)

				// Also try to get previous logs if any
				prevLogs := runCmd("kubectl", "logs", pod, "-n", ns, "-c", container, "--previous")
				if !strings.Contains(prevLogs, "Command failed") {
					h.saveArtifact(fmt.Sprintf("logs/%s/%s-%s-previous.log", ns, pod, container), prevLogs)
				}
			}
		}
	}
}

func (h *Harness) saveArtifact(name, content string) {
	dir := os.Getenv("ARTIFACTS")
	if dir == "" {
		dir = "/tmp/artifacts"
	}
	path := filepath.Join(dir, "otel-e2e", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, []byte(content), 0o644)
	}
}
