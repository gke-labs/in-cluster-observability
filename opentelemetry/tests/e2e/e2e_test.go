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
	"testing"
	"time"
)

func TestOpenTelemetryHPA(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("E2E env var not set, skipping")
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

	h.WaitForDeployment("test-app", "default", 2*time.Minute)

	// Verify HPA works. The query server is hardcoded to return 100 for test_metric,
	// and target is 50. So it should scale to 2 replicas.
	// HPA might take some time to react.
	h.WaitForReplicas("test-app", "default", "2", 5*time.Minute)
}
