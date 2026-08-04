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
	"strings"
	"testing"
	"time"
)

// TestTLSDecryptGoCryptoTLS is the #106 gate: OBI's Go-TLS uprobes
// recover plaintext HTTP from a Go crypto/tls server without any proxy,
// sidecar, or enablement flag. We stand up an HTTPS-only Go echo server
// (nothing listens in plaintext) with a client looping HTTPS requests
// through the Service, then assert the same L7 series a plaintext
// workload would produce — http.server.request.duration attributed to
// the TLS server pod. Its presence is the proof: without decrypt OBI
// would only ever see opaque L4 TCP bytes on 8443.
func TestTLSDecryptGoCryptoTLS(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set, skipping e2e test")
	}

	repoRoot := GitRoot(t)
	h := NewHarness(t, "ollie-e2e")
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	h.InstallOllie(repoRoot)
	h.BuildTLSServerImage(repoRoot)
	h.DeployTLSGoWorkload()

	assertHTTPServerSeriesForPod(t, h, "tls-go", "tls-go-", 19091)
}

// TestTLSDecryptOpenSSL is the #107 gate: OBI's OpenSSL (libssl.so)
// uprobes recover plaintext HTTP from a stock, dynamically-linked nginx
// serving HTTPS. Same shape as the Go case; the assertion targets the
// nginx *server* pod, which isolates the OpenSSL decrypt path (the Go
// client's own crypto/tls would surface as http.client on the client
// pod, not http.server on nginx).
//
// Note the BoringSSL caveat (ADR-0031, docs/design/data-plane.md §4):
// nginx here uses system OpenSSL. A statically BoringSSL-linked binary
// exposes no libssl.so uprobe target and would not be decryptable — we
// deliberately do not assert that negative here.
func TestTLSDecryptOpenSSL(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set, skipping e2e test")
	}

	repoRoot := GitRoot(t)
	h := NewHarness(t, "ollie-e2e")
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	h.InstallOllie(repoRoot)
	h.BuildTLSServerImage(repoRoot)
	h.DeployTLSOpenSSLWorkload()

	assertHTTPServerSeriesForPod(t, h, "tls-nginx", "tls-nginx-", 19092)
}

// assertHTTPServerSeriesForPod forwards the agent on the node running the
// app=<appLabel> pod and polls its :9090 scrape surface for an
// http.server.request.duration series carrying the workload pod's K8s
// identity (k8s_pod_name starting with podPrefix). Mirrors
// TestCaptureSmokeHTTP's node-targeting: the cluster is multi-node and
// each agent only sees its own node's traffic, so `ds/ollie-agent` would
// coin-flip the result.
func assertHTTPServerSeriesForPod(t *testing.T, h *Harness, appLabel, podPrefix string, localPort int) {
	t.Helper()

	node, err := h.KubectlOutput("get", "pod", "-n", "default", "-l", "app="+appLabel,
		"-o", "jsonpath={.items[0].spec.nodeName}")
	if err != nil {
		t.Fatalf("%s node: %v", appLabel, err)
	}
	agentPod, err := h.KubectlOutput("get", "pod", "-n", "ollie-system",
		"-l", "app.kubernetes.io/component=agent",
		"--field-selector", "spec.nodeName="+strings.TrimSpace(node),
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		t.Fatalf("agent pod on %s: %v", node, err)
	}
	base := h.PortForward("pod/"+strings.TrimSpace(agentPod), "ollie-system", localPort, 9090)

	// Accept any Prometheus rendering of http.server.request.duration
	// (suffix variants differ across exporter versions), same as the
	// plaintext smoke test.
	h.PollHTTP(base+"/metrics", 3*time.Minute,
		"OBI-decrypted HTTP series with k8s identity for the "+appLabel+" pod",
		func(body string) bool {
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "#") {
					continue
				}
				if strings.Contains(line, "http_server_request_duration") &&
					strings.Contains(line, `k8s_pod_name="`+podPrefix) {
					return true
				}
			}
			return false
		})
}
