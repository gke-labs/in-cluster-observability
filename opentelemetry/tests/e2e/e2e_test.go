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
	"bufio"
	"crypto/tls"
	"encoding/json"
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

func TestOpenTelemetryHPA(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E env var not set, skipping")
	}

	h := NewHarness(t, "otel-e2e")
	h.Setup()

	gitRoot := h.GetGitRoot()
	otelRoot := filepath.Join(gitRoot, "opentelemetry")

	// Build and load images
	h.DockerBuild("opentelemetry-node-agent:e2e", filepath.Join(otelRoot, "images/opentelemetry-node-agent/Dockerfile"), otelRoot)
	h.KindLoad("opentelemetry-node-agent:e2e")

	h.DockerBuild("opentelemetry-query-server:e2e", filepath.Join(otelRoot, "images/opentelemetry-query-server/Dockerfile"), otelRoot)
	h.KindLoad("opentelemetry-query-server:e2e")

	h.DockerBuild("test-app:e2e", filepath.Join(otelRoot, "images/test-app/Dockerfile"), otelRoot)
	h.KindLoad("test-app:e2e")

	h.DockerBuild("test-client:e2e", filepath.Join(otelRoot, "images/test-client/Dockerfile"), otelRoot)
	h.KindLoad("test-client:e2e")

	// Install cert-manager
	h.KubectlApplyFile("https://github.com/cert-manager/cert-manager/releases/download/v1.14.4/cert-manager.yaml")
	h.WaitForDeployment("cert-manager", "cert-manager", 2*time.Minute)
	h.WaitForDeployment("cert-manager-cainjector", "cert-manager", 2*time.Minute)
	h.WaitForDeployment("cert-manager-webhook", "cert-manager", 2*time.Minute)

	// Deploy core components
	h.KubectlApplyFile(filepath.Join(otelRoot, "k8s/manifest.yaml"))

	// Patch components to use e2e images and set imagePullPolicy to Never
	h.RunCommand("kubectl", "set", "image", "daemonset/opentelemetry-node-agent", "opentelemetry-node-agent=opentelemetry-node-agent:e2e", "-n", "observability-system")
	h.RunCommand("kubectl", "patch", "daemonset", "opentelemetry-node-agent", "-n", "observability-system", "--type=json", "-p", `[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]`)

	h.RunCommand("kubectl", "set", "image", "deployment/query-server", "query-server=opentelemetry-query-server:e2e", "-n", "observability-system")
	h.RunCommand("kubectl", "patch", "deployment", "query-server", "-n", "observability-system", "--type=json", "-p", `[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]`)

	// Wait for core components
	h.WaitForDaemonSet("opentelemetry-node-agent", "observability-system", 2*time.Minute)
	h.WaitForDeployment("query-server", "observability-system", 2*time.Minute)

	// Deploy test app
	h.KubectlApplyFile(filepath.Join(otelRoot, "tests/e2e/testdata/simple-hpa/manifest.yaml"))

	// Patch test app to use e2e image
	h.RunCommand("kubectl", "set", "image", "deployment/test-app", "test-app=test-app:e2e", "-n", "default")
	h.RunCommand("kubectl", "patch", "deployment", "test-app", "-n", "default", "--type=json", "-p", `[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]`)

	h.RunCommand("kubectl", "set", "image", "deployment/test-client", "test-client=test-client:e2e", "-n", "default")
	h.RunCommand("kubectl", "patch", "deployment", "test-client", "-n", "default", "--type=json", "-p", `[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]`)

	h.WaitForDeployment("test-app", "default", 2*time.Minute)
	h.WaitForDeployment("test-client", "default", 2*time.Minute)

	// Verify HPA works. The test-client is sending 100 QPS,
	// and target is 50. So it should scale to 2 replicas.
	// HPA might take some time to react.
	h.WaitForReplicas("test-app", "default", "2", 5*time.Minute)

	// Verify logs search API works.
	// 1. Establish port-forward to queryserver service
	localPort := 18443
	pfCmd := exec.Command("kubectl", "port-forward", "-n", "observability-system", "service/queryserver", fmt.Sprintf("%d:443", localPort))
	stdout, err := pfCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to create port-forward stdout pipe: %v", err)
	}
	pfCmd.Stderr = os.Stderr
	if err := pfCmd.Start(); err != nil {
		t.Fatalf("failed to start port-forward to queryserver: %v", err)
	}
	defer func() {
		_ = pfCmd.Process.Kill()
		_ = pfCmd.Wait()
	}()

	// Wait for port-forward to be ready
	readyChan := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("port-forward: %s", line)
			if strings.Contains(line, "Forwarding from") {
				readyChan <- struct{}{}
				return
			}
		}
	}()

	select {
	case <-readyChan:
		t.Log("port-forward to queryserver is ready")
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for port-forward to queryserver to be ready")
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	startStr := time.Now().Add(-15 * time.Minute).Format(time.RFC3339)
	endStr := time.Now().Add(5 * time.Minute).Format(time.RFC3339)

	// Query 1: Full-text search
	query1 := "HTTP"
	url1 := fmt.Sprintf("https://127.0.0.1:%d/api/logs/search?q=%s&start=%s&end=%s", localPort, query1, startStr, endStr)
	resp1, err := client.Get(url1)
	if err != nil {
		t.Fatalf("failed to GET full-text search: %v", err)
	}
	defer resp1.Body.Close()

	body1, err := io.ReadAll(resp1.Body)
	if err != nil {
		t.Fatalf("failed to read full-text search response body: %v", err)
	}
	t.Logf("Full-text query response: %s", string(body1))

	var results1 []map[string]any
	if err := json.Unmarshal(body1, &results1); err != nil {
		t.Fatalf("failed to unmarshal full-text query results: %v\nResponse: %s", err, string(body1))
	}
	if len(results1) == 0 {
		t.Errorf("expected some logs matching full-text query %q, got 0", query1)
	} else {
		t.Logf("Successfully verified full-text search: got %d logs", len(results1))
	}

	// Query 2: namespace= structured query
	query2 := "namespace=default"
	url2 := fmt.Sprintf("https://127.0.0.1:%d/api/logs/search?q=%s&start=%s&end=%s", localPort, query2, startStr, endStr)
	resp2, err := client.Get(url2)
	if err != nil {
		t.Fatalf("failed to GET namespace structured search: %v", err)
	}
	defer resp2.Body.Close()

	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("failed to read namespace structured search response body: %v", err)
	}
	t.Logf("Namespace structured query response: %s", string(body2))

	var results2 []map[string]any
	if err := json.Unmarshal(body2, &results2); err != nil {
		t.Fatalf("failed to unmarshal namespace query results: %v\nResponse: %s", err, string(body2))
	}
	if len(results2) == 0 {
		t.Errorf("expected some logs matching structured query %q, got 0", query2)
	} else {
		// Verify that all results indeed have namespace == "default"
		for _, r := range results2 {
			if r["namespace"] != "default" {
				t.Errorf("expected log namespace to be 'default', got %q", r["namespace"])
			}
		}
		t.Logf("Successfully verified namespace structured search: got %d logs", len(results2))
	}
}
