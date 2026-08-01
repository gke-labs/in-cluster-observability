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

// TestQueryFanout is the #94/#95 end-to-end gate: the query server
// discovers the agent through the headless Service, fans a PromQL
// query out to the agent's remote-read endpoint, and returns
// cluster-aggregated results over the Prometheus-compatible HTTP
// API. Kind runs one node, so "cluster total" degenerates to one
// agent's data — the multi-agent aggregation and degraded semantics
// are covered by internal/fanout's in-process tests; this asserts
// the deployed wiring (DNS discovery, token auth agent-side,
// remote-read transport, API surface).
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
