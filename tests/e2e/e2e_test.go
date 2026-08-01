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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

	repoRoot := GitRoot(t)
	h := NewHarness(t, "ollie-e2e")
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	h.InstallOllie(repoRoot)
	h.DeployTestWorkload()

	// Assert on the agent's production scrape surface. port-forward
	// terminates on the pod's loopback, which --scrape-auth exempts by
	// design (#145), so no token is needed here; in-cluster scrapes
	// still require one.
	//
	// The cluster is multi-node (#194) and each agent only sees its
	// own node's traffic, so forward the agent on the echo pod's node
	// specifically — `ds/ollie-agent` picks an arbitrary pod and would
	// coin-flip this test.
	echoNode, err := h.KubectlOutput("get", "pod", "-n", "default", "-l", "app=echo",
		"-o", "jsonpath={.items[0].spec.nodeName}")
	if err != nil {
		t.Fatalf("echo node: %v", err)
	}
	agentPod, err := h.KubectlOutput("get", "pod", "-n", "ollie-system",
		"-l", "app.kubernetes.io/component=agent",
		"--field-selector", "spec.nodeName="+strings.TrimSpace(echoNode),
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		t.Fatalf("agent pod on %s: %v", echoNode, err)
	}
	base := h.PortForward("pod/"+strings.TrimSpace(agentPod), "ollie-system", 19090, 9090)

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

// TestQueryFanout is the #94/#95 end-to-end gate: the query server
// discovers the agent through the headless Service, fans a PromQL
// query out to the agent's remote-read endpoint, and returns
// cluster-aggregated results over the Prometheus-compatible HTTP
// API. This asserts the deployed wiring end to end (DNS discovery,
// token auth agent-side, remote-read transport, API surface); the
// cross-node merge and degraded-on-agent-loss semantics on the real
// multi-node topology are asserted by TestMultiNodeFanout and
// TestDegradedOnAgentLoss.
func TestQueryFanout(t *testing.T) {
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
	h.DeployTestWorkload()

	// port-forward terminates on the pod loopback, which --api-auth
	// exempts by design; in-cluster consumers need a token bound to
	// ollie-promql-reader.
	base := h.PortForward("deploy/ollie-query", "ollie-system", 19095, 9095)

	// The store self-scrapes every second, so ollie_agent_up reaches
	// the tsdb within seconds of agent boot; sum() proves engine +
	// fan-out + merge, not just passthrough.
	h.PollHTTP(base+`/api/v1/query?query=sum(ollie_agent_up)`, 2*time.Minute,
		"query server returns sum(ollie_agent_up) ≥ 1",
		func(body string) bool {
			return strings.Contains(body, `"status":"success"`) &&
				strings.Contains(body, `"resultType":"vector"`) &&
				strings.Contains(body, `"value":[`) &&
				!strings.Contains(body, `"result":[]`)
		})

	// An OBI-captured, K8s-attributed L7 series is queryable through
	// the full path: OBI -> agent forwarder -> registry self-scrape
	// -> tsdb -> remote read -> fan-out -> PromQL.
	obiQuery := url.QueryEscape(
		`sum(rate({__name__=~"http_server_request_duration.*_count",k8s_namespace_name="default"}[1m]))`)
	h.PollHTTP(base+"/api/v1/query?query="+obiQuery,
		3*time.Minute,
		"query server aggregates OBI HTTP series for the echo workload",
		func(body string) bool {
			return strings.Contains(body, `"status":"success"`) &&
				!strings.Contains(body, `"result":[]`)
		})
}

// TestHPACustomMetrics is #96's end-to-end gate and the v0.5
// vertical-slice capstone: the aggregated custom.metrics.k8s.io API
// serves OBI-derived metrics, and an HPA scales a workload on them.
// Full path under test: OBI capture -> agent tsdb -> remote-read
// fan-out -> PromQL -> custom-metrics adapter -> kube-aggregator ->
// HPA controller -> Deployment scale.
func TestHPACustomMetrics(t *testing.T) {
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
	h.DeployTestWorkload()

	// The aggregator marks the APIService Available once it can
	// reach the backend over TLS.
	h.PollKubectl(2*time.Minute, "APIService v1beta1.custom.metrics.k8s.io Available",
		func() (string, error) {
			return h.KubectlOutput("get", "apiservice", "v1beta1.custom.metrics.k8s.io",
				"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`)
		},
		func(out string) bool { return strings.TrimSpace(out) == "True" })

	// #96 acceptance: the raw GET returns a MetricValueList. Both
	// the bare-plural form from the issue text and the grouped form
	// the HPA uses.
	for _, res := range []string{"deployments", "deployments.apps"} {
		path := "/apis/custom.metrics.k8s.io/v1beta1/namespaces/default/" + res + "/echo/qps"
		h.PollKubectl(4*time.Minute, "custom metric qps for deployment echo via "+res,
			func() (string, error) { return h.KubectlOutput("get", "--raw", path) },
			func(out string) bool {
				return strings.Contains(out, `"kind":"MetricValueList"`) &&
					strings.Contains(out, `"metricName":"qps"`)
			})
	}

	// The HPA demo: scale echo on qps per pod. traffic-client drives
	// a steady few requests per second; averageValue 500m (0.5 rps
	// per replica) forces a scale-up from 1 without needing precise
	// load numbers.
	h.ApplyStdin(`
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: echo
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: echo
  minReplicas: 1
  maxReplicas: 3
  metrics:
    - type: Object
      object:
        describedObject:
          apiVersion: apps/v1
          kind: Deployment
          name: echo
        metric:
          name: qps
        target:
          type: AverageValue
          averageValue: 500m
`)

	h.PollKubectl(6*time.Minute, "HPA scales echo above 1 replica on qps",
		func() (string, error) {
			return h.KubectlOutput("get", "deployment", "echo", "-n", "default",
				"-o", "jsonpath={.status.replicas}")
		},
		func(out string) bool {
			out = strings.TrimSpace(out)
			return out == "2" || out == "3"
		})
}

// TestOTLPExport is #97/#98's end-to-end gate: the agent relays the
// raw OTLP it receives from OBI to an in-cluster OpenTelemetry
// collector, and a dead endpoint degrades to counted drops without
// touching capture.
func TestOTLPExport(t *testing.T) {
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

	// A minimal collector: OTLP in, debug-log out.
	h.PullAndLoad(collectorImage)
	h.ApplyStdin(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: otelcol-config
  namespace: default
data:
  config.yaml: |
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: 0.0.0.0:4317
    exporters:
      debug:
        verbosity: normal
    service:
      pipelines:
        metrics:
          receivers: [otlp]
          exporters: [debug]
        traces:
          receivers: [otlp]
          exporters: [debug]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: otel-collector
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: otel-collector}
  template:
    metadata:
      labels: {app: otel-collector}
    spec:
      containers:
        - name: collector
          image: ` + collectorImage + `
          imagePullPolicy: Never
          args: ["--config=/etc/otelcol/config.yaml"]
          volumeMounts:
            - {name: config, mountPath: /etc/otelcol}
      volumes:
        - name: config
          configMap: {name: otelcol-config}
---
apiVersion: v1
kind: Service
metadata:
  name: otel-collector
  namespace: default
spec:
  selector: {app: otel-collector}
  ports:
    - {name: otlp-grpc, port: 4317, targetPort: 4317}
`)
	h.WaitRollout("deployment", "otel-collector", "default", 2*time.Minute)

	// Point the agent's relay at it and generate traffic.
	h.KubectlRetry(3, "patch", "daemonset", "ollie-agent", "-n", "ollie-system", "--type=json",
		"-p", `[{"op":"add","path":"/spec/template/spec/containers/1/args/-","value":"--export-otlp-endpoint=otel-collector.default.svc.cluster.local:4317"}]`)
	h.WaitRollout("daemonset", "ollie-agent", "ollie-system", 5*time.Minute)
	h.DeployTestWorkload()

	// The debug exporter logs one line per received batch.
	h.PollKubectl(4*time.Minute, "collector logs OTLP metrics from the agent",
		func() (string, error) {
			return h.KubectlOutput("logs", "-n", "default", "deploy/otel-collector", "--tail=200")
		},
		func(out string) bool { return strings.Contains(out, "data points") })
	h.PollKubectl(3*time.Minute, "collector logs OTLP traces from the agent",
		func() (string, error) {
			return h.KubectlOutput("logs", "-n", "default", "deploy/otel-collector", "--tail=200")
		},
		func(out string) bool {
			// verbosity-dependent: summary lines carry "spans", the
			// detailed form logs one attribute line per span.
			return strings.Contains(out, "spans") || strings.Contains(out, "http.request.method")
		})

	// Kill the collector: the relay degrades to accounted drops and
	// the agent stays alive (#97 acceptance).
	h.Kubectl("scale", "deployment/otel-collector", "-n", "default", "--replicas=0")
	base := h.PortForward("ds/ollie-agent", "ollie-system", 19092, 9090)
	h.PollHTTP(base+"/metrics", 4*time.Minute, "export drops counted after collector death",
		func(body string) bool {
			return strings.Contains(body, "ollie_export_dropped_total") ||
				strings.Contains(body, `ollie_export_errors_total`)
		})
	// Capture is unharmed: the scrape surface still serves.
	h.PollHTTP(base+"/metrics", 1*time.Minute, "agent alive after export endpoint death",
		func(body string) bool { return strings.Contains(body, "ollie_agent_up") })
}

// TestIobsctl is #100's gate and doubles as #99's deployed-wiring
// check: the CLI reaches the query server over an automatic
// port-forward, PromQL returns a table, and a CEL-filtered span
// subscription streams live spans multiplexed from the agent's
// stream service.
func TestIobsctl(t *testing.T) {
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
	h.DeployTestWorkload()

	bin := filepath.Join(t.TempDir(), "iobsctl")
	h.Run("go", "build", "-o", bin, repoRoot+"/cmd/iobsctl")

	kubectx := "kind-" + h.ClusterName

	// PromQL through the CLI: ollie_agent_up lands in the store
	// within seconds of boot.
	h.PollKubectl(3*time.Minute, "iobsctl metrics returns sum(ollie_agent_up)",
		func() (string, error) {
			out, err := exec.Command(bin, "metrics",
				"--context", kubectx, "--timeout", "30s",
				"sum(ollie_agent_up)").CombinedOutput()
			return string(out), err
		},
		func(out string) bool {
			// #188: the header prints even for an empty result, so
			// requiring it alone was false-green. Demand a data row
			// whose value column is a number >= 1 (each node's
			// ollie_agent_up contributes 1 to the sum).
			if !strings.Contains(out, "METRIC") {
				return false
			}
			for _, line := range strings.Split(out, "\n")[1:] {
				for _, f := range strings.Fields(line) {
					if v, err := strconv.ParseFloat(f, 64); err == nil && v >= 1 {
						return true
					}
				}
			}
			return false
		})

	// Live CEL span stream: the echo workload produces HTTP server
	// spans in the default namespace continuously; --max 1 exits on
	// the first match.
	h.PollKubectl(4*time.Minute, "iobsctl spans streams a CEL-matched span",
		func() (string, error) {
			out, err := exec.Command(bin, "spans",
				"--filter", `resource["k8s.namespace.name"] == "default"`,
				"--max", "1", "--output", "json",
				"--context", kubectx, "--timeout", "60s").CombinedOutput()
			return string(out), err
		},
		func(out string) bool {
			return strings.Contains(out, `"span"`) && strings.Contains(out, `"k8s.namespace.name":"default"`)
		})
}

// TestMultiNodeFanout is the #194 gate: on the real multi-node cluster
// the query server must fan out to every node's agent and merge their
// series while keeping same-named series distinct per node. The v0.5
// cross-node merge bug (ChainedSeriesMerge collapsing identical
// series) shipped with green CI precisely because the old e2e cluster
// was single-node, where this is unobservable. Each agent stamps its
// own k8s_node_name (internal/store/ingest.go) onto ollie_agent_up, so
// a correct merge yields exactly one series per node.
func TestMultiNodeFanout(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set, skipping e2e test")
	}

	repoRoot := GitRoot(t)
	h := NewHarness(t, sharedClusterName)
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	h.InstallOllie(repoRoot)

	base := h.PortForward("deploy/ollie-query", "ollie-system", 19096, 9095)

	// One ollie_agent_up series per node survives the fan-out merge: a
	// merge that deduped across nodes would return 1.
	h.PollHTTP(base+"/api/v1/query?query="+url.QueryEscape("count(ollie_agent_up)"),
		2*time.Minute, "fan-out counts one ollie_agent_up per node",
		func(body string) bool {
			v, ok := vectorScalar(body)
			return ok && v == float64(clusterNodes)
		})

	// The node label itself is distinct across the merged set — the
	// direct assertion that per-node identity is preserved.
	h.PollHTTP(base+"/api/v1/query?query="+url.QueryEscape("count(count by (k8s_node_name)(ollie_agent_up))"),
		2*time.Minute, "ollie_agent_up carries a distinct k8s_node_name per node",
		func(body string) bool {
			v, ok := vectorScalar(body)
			return ok && v == float64(clusterNodes)
		})
}

// TestAuthBoundaries is the #195 gate for the two auth boundaries that
// port-forward can never exercise (loopback is exempt by design, #145):
// the query front-proxy's mTLS on :6443 and the agent scrape token on
// :9090. Both are probed from a bare pod in the default namespace with
// no ollie credentials.
func TestAuthBoundaries(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set, skipping e2e test")
	}

	repoRoot := GitRoot(t)
	h := NewHarness(t, sharedClusterName)
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	h.InstallOllie(repoRoot)
	h.BuildProbeImage(repoRoot)

	// Create the permitted scraper namespace up front (used by boundary
	// 2b) so kindnet's NetworkPolicy controller has synced its
	// kubernetes.io/metadata.name label into its informer cache well
	// before the probe that relies on it runs.
	h.EnsureNamespace("gmp-system")

	// Boundary 1 — the :6443 front-proxy requires a client cert chaining
	// to the requestheader CA (RequireAndVerifyClientCert). A certless
	// bare pod must be rejected — the auth bypass closed in v0.5.1 (#180).
	// The probe issues a full HTTPS round-trip, not a bare handshake:
	// under TLS 1.3 the server delivers its client-cert rejection as a
	// post-handshake alert, so the handshake itself reports success and
	// the rejection only surfaces on the first read (the HTTP request).
	// TCP must connect (ollie-query-ingress opens :6443 to any source, so
	// the aggregator can reach it — identity is the cert, not the network
	// position), proving the rejection is TLS/auth-level. A certless
	// caller must never get 200; acceptable outcomes are a TLS-layer
	// rejection (HTTPS_ERROR, "certificate required" on TLS 1.3) or, were
	// the boundary ever moved to the HTTP layer, a 401/403.
	out := h.RunProbe("default", "probe-mtls", "mtls", "https://ollie-query.ollie-system.svc.cluster.local:6443/apis/custom.metrics.k8s.io/v1beta1")
	rejected := strings.Contains(out, "HTTPS_ERROR") ||
		strings.Contains(out, "HTTPS_STATUS 401") ||
		strings.Contains(out, "HTTPS_STATUS 403")
	if !strings.Contains(out, "TCP_OK") || !rejected || strings.Contains(out, "HTTPS_STATUS 200") {
		t.Fatalf("expected TCP_OK + certless rejection (HTTPS_ERROR or 401/403, never 200) from :6443 mTLS; got: %s", out)
	}

	agentIP, err := h.KubectlOutput("get", "pod", "-n", "ollie-system",
		"-l", "app.kubernetes.io/component=agent",
		"-o", "jsonpath={.items[0].status.podIP}")
	if err != nil {
		t.Fatalf("agent pod IP: %v", err)
	}
	agentIP = strings.TrimSpace(agentIP)

	// Boundary 2a — NetworkPolicy (#143). The agent scrape port :9090
	// carries cross-namespace k8s.* identity and traffic volumes;
	// ollie-agent-ingress pins its ingress to the scraper namespace
	// (gmp-system by default) via the immutable kubernetes.io/metadata.name
	// label. A pod in an unrelated namespace (default) must be DROPPED at
	// the network layer — the SYN is discarded, so a bare TCP dial times
	// out rather than connecting. kindnet (KIND's default CNI) enforces
	// NetworkPolicy via nftables on the node images this suite runs, so
	// this is a live assertion, not a no-op. A TCP_OK here would mean the
	// policy is not being enforced.
	out = h.RunProbe("default", "probe-np", "tcp",
		fmt.Sprintf("%s:9090", agentIP))
	if !strings.Contains(out, "TCP_FAIL") {
		t.Fatalf("expected TCP_FAIL to :9090 from an unpermitted namespace (NetworkPolicy #143 must drop it); got: %s", out)
	}

	// Boundary 2b — bearer token (#145). From the permitted scraper
	// namespace the packet reaches the listener, so the token layer is
	// what stands between an unauthenticated caller and the metrics: a
	// tokenless GET must be 401. This isolates the token check from the
	// network check above — without the permitted namespace the request
	// would be dropped before auth ever ran (which is exactly what bit an
	// earlier version of this test). gmp-system was created up front.
	out = h.RunProbe("gmp-system", "probe-scrape", "get",
		fmt.Sprintf("http://%s:9090/metrics", agentIP))
	if !strings.Contains(out, "STATUS 401") {
		t.Fatalf("expected STATUS 401 from tokenless :9090 scrape in the permitted namespace; got: %s", out)
	}
}

// TestDegradedOnAgentLoss is the #195 gate for fan-out degradation on
// the deployed topology: when a discovered agent stops answering, the
// query API must surface degraded=true rather than silently returning a
// partial result. Killing one agent leaves its endpoint in the query
// server's resolved target set until the next discovery tick
// (--resolve-interval, 15s); every fan-out in that window hits a dead
// target and is marked degraded. The window is transient by design, so
// this polls tight and asserts it is observed at least once.
func TestDegradedOnAgentLoss(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("RUN_E2E not set, skipping e2e test")
	}

	repoRoot := GitRoot(t)
	h := NewHarness(t, sharedClusterName)
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	h.InstallOllie(repoRoot)

	base := h.PortForward("deploy/ollie-query", "ollie-system", 19097, 9095)

	// Baseline: both agents present and the query is not degraded.
	h.PollHTTP(base+"/api/v1/query?query="+url.QueryEscape("count(ollie_agent_up)"),
		2*time.Minute, "baseline: fan-out sees both agents, not degraded",
		func(body string) bool {
			v, ok := vectorScalar(body)
			return ok && v == float64(clusterNodes) && !strings.Contains(body, `"degraded":true`)
		})

	// Restore the DaemonSet to full health before any later test in the
	// shared cluster observes a missing agent, however this test exits.
	t.Cleanup(func() {
		h.Kubectl("rollout", "status", "daemonset/ollie-agent", "-n", "ollie-system", "--timeout=5m")
	})

	victim, err := h.KubectlOutput("get", "pod", "-n", "ollie-system",
		"-l", "app.kubernetes.io/component=agent",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		t.Fatalf("victim agent pod: %v", err)
	}
	victim = strings.TrimSpace(victim)
	h.Kubectl("delete", "pod", victim, "-n", "ollie-system", "--grace-period=1", "--wait=false")

	deadline := time.Now().Add(30 * time.Second)
	var last string
	saw := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/query?query=" + url.QueryEscape("count(ollie_agent_up)"))
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			last = string(b)
			if strings.Contains(last, `"degraded":true`) {
				saw = true
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !saw {
		t.Fatalf("never observed degraded=true in the 30s after killing agent %s; last response: %s", victim, last)
	}
}

// vectorScalar extracts the single sample value from a Prometheus
// instant-vector query response (e.g. the result of count(...)). It
// returns false unless the response is a success carrying exactly one
// vector sample, so pollers can treat a not-yet-populated result as
// "keep waiting" rather than a hard failure.
func vectorScalar(body string) (float64, bool) {
	var env struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return 0, false
	}
	if env.Status != "success" || env.Data.ResultType != "vector" || len(env.Data.Result) != 1 {
		return 0, false
	}
	var s string
	if err := json.Unmarshal(env.Data.Result[0].Value[1], &s); err != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
