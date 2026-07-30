---
title: "Getting started"
linkTitle: "Getting started"
weight: 10
description: "Deploy Ollie on a Kind cluster, attach to a real workload, and see per-pod L4 + L7 metrics with K8s identity in under 10 minutes."
---

This walks from a fresh clone to per-pod HTTP metrics on a real nginx Deployment, scraped from the agent's `:9090` endpoint with `k8s.pod.name` / `k8s.namespace.name` / `k8s.deployment.name` labels attached by OBI.

Kind is the reference cluster — the same flow works on any Linux 6.x cluster with eBPF support (GKE 1.33+ on cos-125+, EKS with `bottlerocket`, k3s with `--snapshotter=overlayfs`, etc.); only the image-loading step differs.

## Prerequisites

- A clone of [`github.com/gke-labs/in-cluster-observability`](https://github.com/gke-labs/in-cluster-observability).
- `docker`, `kubectl`, `kind`, `go` 1.26+.
- A kernel with BTF + CO-RE (any 5.10+ kernel built with `CONFIG_DEBUG_INFO_BTF=y`; Kind's default node image qualifies).

## 1. Spin up a Kind cluster

```sh
kind create cluster --name ollie-v03
```

The default single-node cluster is enough. Multi-node works too — the agent is a DaemonSet, you'll get one agent per node.

## 2. Build and load the images

Since v0.5 the install is three Ollie workloads — the agent DaemonSet,
the controller, and the query server (which also backs the HPA
custom-metrics API) — so three images build from this repo, plus the
pinned OBI image:

```sh
# Build the three Ollie images.
docker build -t ollie:dev            -f images/ollie/Dockerfile .
docker build -t ollie-controller:dev -f images/ollie-controller/Dockerfile .
docker build -t ollie-query:dev      -f images/ollie-query/Dockerfile .

# Pull OBI (the eBPF data plane).
docker pull otel/ebpf-instrument:v0.10.0

# Load all four into the Kind cluster's node-local image store.
kind load docker-image --name ollie-v03 ollie:dev
kind load docker-image --name ollie-v03 ollie-controller:dev
kind load docker-image --name ollie-v03 ollie-query:dev
kind load docker-image --name ollie-v03 otel/ebpf-instrument:v0.10.0
```

On a real cluster, push to your registry instead (`docker push <registry>/ollie:dev`, etc.) and skip the `kind load` steps.

## 3. Apply the manifest

```sh
kubectl apply -k k8s/
```

This installs:

- Namespace `ollie-system`.
- ServiceAccount + ClusterRole + ClusterRoleBinding granting `list,watch` on `pods`, `services`, `nodes`, and `replicasets` — required so OBI's K8s metadata informer can attach `k8s.*` attributes to captured events.
- DaemonSet `ollie-agent` with two containers:
  - `obi` — the upstream `otel/ebpf-instrument:v0.10.0` image, privileged with the eBPF capability set, doing the actual eBPF capture.
  - `agent` — our image, unprivileged, exposing the Prometheus scrape on `:9090`.
- Deployment `ollie-controller` — the control plane: reconciles the `TrafficMonitor` / `ClusterTrafficPolicy` CRDs (also installed) and pushes capture config to agents.
- Deployment `ollie-query` — cluster-wide PromQL over every agent's store (`:9095`), plus the `custom.metrics.k8s.io` APIService that lets an HPA scale on captured metrics.
- NetworkPolicies (default-deny ingress with minimum allows) and the custom-metrics RBAC.

## 4. Pin images on Kind

On Kind, locally loaded images won't be re-pulled, so every workload needs `imagePullPolicy: IfNotPresent`. The manifests intentionally leave the policy unset (an `ap deploy` convention for production), so patch all three explicitly on Kind:

```sh
kubectl patch -n ollie-system daemonset/ollie-agent --type=strategic -p='{
  "spec":{"template":{"spec":{"containers":[
    {"name":"obi",   "image":"otel/ebpf-instrument:v0.10.0", "imagePullPolicy":"IfNotPresent"},
    {"name":"agent", "image":"ollie:dev",                    "imagePullPolicy":"IfNotPresent"}
  ]}}}
}'
kubectl patch -n ollie-system deployment/ollie-controller --type=strategic -p='{
  "spec":{"template":{"spec":{"containers":[
    {"name":"controller", "image":"ollie-controller:dev", "imagePullPolicy":"IfNotPresent"}
  ]}}}
}'
kubectl patch -n ollie-system deployment/ollie-query --type=strategic -p='{
  "spec":{"template":{"spec":{"containers":[
    {"name":"query", "image":"ollie-query:dev", "imagePullPolicy":"IfNotPresent"}
  ]}}}
}'

kubectl rollout status -n ollie-system daemonset/ollie-agent --timeout=120s
kubectl rollout status -n ollie-system deployment/ollie-controller --timeout=120s
kubectl rollout status -n ollie-system deployment/ollie-query --timeout=120s
```

Skip this step on a real cluster — the default `Always` (or `IfNotPresent` when not using `:latest`) does the right thing once your images are in a real registry.

## 5. Sanity check: the agent is up

```sh
AGENT_POD=$(kubectl get pod -n ollie-system -l app.kubernetes.io/component=agent \
  -o jsonpath='{.items[0].metadata.name}')

kubectl logs -n ollie-system "$AGENT_POD" -c agent | head -5
```

You should see:

```
ollie v0.3.0-dev
v0.3: OTLP receiver + OBI config writer; OBI does K8s enrichment (per ADR-0021)
OTLP receiver: gRPC=127.0.0.1:4317 HTTP=127.0.0.1:4318; OBI config: /etc/ollie/obi-config/config.yaml
OBI smoke-test discovery seeded: open_ports=80,443,8080,8443
scrape endpoint: http://0.0.0.0:9090/metrics
```

The `OBI smoke-test discovery seeded` line is critical — it means the agent is telling OBI to attach to any process listening on the listed ports. (Once v0.4 ships the controller, CRDs will drive this instead of a flag.)

## 6. Deploy a workload

```sh
kubectl create namespace demo
kubectl create deployment nginx --image=nginx:1.27 -n demo
kubectl expose deployment nginx --port=80 -n demo
kubectl rollout status -n demo deployment/nginx --timeout=60s
```

Wait ~30s after the pod is `Running` — OBI's discovery loop takes a couple cycles to spot the new process. Check the obi container's logs to confirm it attached:

```sh
kubectl logs -n ollie-system "$AGENT_POD" -c obi | grep instrumenting
```

You should see something like:

```
msg="instrumenting process" component=discover.traceAttacher cmd=/usr/sbin/nginx pid=2417 type=generic service=smoke
```

If you don't see this within a minute, see [Troubleshooting](#troubleshooting) below.

## 7. Drive traffic

```sh
kubectl run -n demo load --rm -it --restart=Never \
  --image=curlimages/curl:8.10.1 -- \
  sh -c 'for i in $(seq 1 200); do curl -s -o /dev/null http://nginx.demo.svc/; done; echo done'
```

## 8. Scrape the agent's `/metrics`

The agent image is distroless/static (no `curl` inside). Use an ephemeral debug container that joins the agent pod's network namespace — `-it` is required, without it `kubectl debug` exits silently:

```sh
# Find the agent pod on the same node as nginx.
NGINX_NODE=$(kubectl get pod -n demo -l app=nginx \
  -o jsonpath='{.items[0].spec.nodeName}')
AGENT_POD=$(kubectl get pod -n ollie-system \
  -l app.kubernetes.io/component=agent \
  --field-selector spec.nodeName="$NGINX_NODE" \
  -o jsonpath='{.items[0].metadata.name}')

# Dump /metrics.
kubectl debug -n ollie-system "$AGENT_POD" \
  --image=curlimages/curl:8.10.1 --target=agent -it -- \
  curl -s http://127.0.0.1:9090/metrics
```

This works without credentials because loopback (pod-internal) requests are exempt from scrape auth. From anywhere else on the cluster network, `/metrics` requires a bearer token — see [Wiring Prometheus](#wiring-prometheus).

## 9. What you should see

Three families of metrics show up:

**Agent self-observability** (always present):

```
ollie_agent_up{version="v0.3.0-dev"} 1
ollie_capture_events_total{kind="metric",module="l4_tcp"} 45
ollie_capture_events_total{kind="span",module="http1"} 600
```

`ollie_capture_events_total` is the bisection point — `module="l4_tcp"` ticks mean OBI's L4 socket filter is firing; `module="http1"` ticks mean OBI's Application mode attached to a process and is producing HTTP spans.

**L4 TCP flows** with dual-sided K8s identity (this is a free win from OBI's network mode):

```
obi_network_flow_bytes{
  direction="response",
  k8s_src_namespace="demo",       k8s_src_owner_name="nginx",       k8s_src_owner_type="Deployment",
  k8s_dst_namespace="ollie-system", k8s_dst_owner_name="ollie-agent", k8s_dst_owner_type="DaemonSet",
  ...
} 122832
```

Both source and destination identity on the same line — you get edge attribution at L4 with no additional work.

**L7 HTTP metrics** from OBI's Application mode, attached to the actual workload pod:

```
http_server_request_duration{
  http_request_method="GET",
  http_response_status_code="200",
  k8s_pod_name="nginx-567b68cc5f-6mggl",
  k8s_namespace_name="demo",
  k8s_deployment_name="nginx",
  k8s_replicaset_name="nginx-567b68cc5f",
  k8s_container_name="nginx",
  k8s_node_name="ollie-v03-control-plane",
  service_name="smoke",
  ...
} 0.068

http_server_request_body_size{...nginx...} 15600
http_server_response_body_size{...nginx...} 170400
```

That's an end-to-end demonstration: nginx received traffic → OBI's eBPF uprobes captured the request → the agent re-exposed it on `:9090` → and every label was attached by OBI's K8s informer with no work from us. See [What works today]({{< relref "what-works-today.md" >}}) for the full list of metrics you can expect.

## Wiring Prometheus

Each agent pod exposes `:9090/metrics` on its pod IP. Point your Prometheus at the agent DaemonSet via a `PodMonitor` (Prometheus Operator), an inline `kubernetes_sd_configs` `pod` role, or a `Service` with `Endpoints` per pod.

Two access controls apply in the default install:

1. **NetworkPolicy** (`k8s/networkpolicy.yaml`) allows scrape ingress only from the `gmp-system` namespace. Running a different scraper? Patch the namespace name in your kustomize overlay (recipe in the manifest comment). No-op on CNIs without policy enforcement (e.g. Kind's default kindnetd).
2. **Bearer-token auth**: the scraper must send a ServiceAccount token authorized for `get` on `/metrics`. GMP's `gmp-system/collector` SA is pre-authorized; for others, add your scraper's SA to the `ollie-metrics-reader` ClusterRoleBinding (`k8s/rbac.yaml`) and configure the scraper to send its token — e.g. for Prometheus Operator, add to the `PodMonitor` endpoint: `authorization: {credentials: ...}` or `bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token`.

A minimal `PodMonitor` (assumes Prometheus Operator is installed):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: ollie-agent
  namespace: ollie-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: agent
  podMetricsEndpoints:
    - port: scrape
      interval: 30s
```

The `scrape` port is named in the DaemonSet manifest, so this works without manual port-number drift.

## Troubleshooting

**The scrape returns no `http_*` metrics.** OBI's Application mode hasn't attached to anything. Common causes:

- The workload binds a port not in `--obi-instrument-ports` (defaults to `80,443,8080,8443`). Tune in `k8s/daemonset.yaml`.
- The traffic ran before OBI's discovery cycle finished. Wait 30s after deploying the workload, then re-drive traffic.
- Required capabilities missing — the manifest grants `SYS_PTRACE`, `CHECKPOINT_RESTORE`, and `DAC_READ_SEARCH` for OBI's L7 attach path. With `OTEL_EBPF_ENFORCE_SYS_CAPS=true` (the default), OBI will crash-loop with a clear "missing capability X" message instead of silently no-opping. Check `kubectl logs -c obi`.

**The scrape returns a `target_info` error.** Means you're on a v0.3 build before commit `fbbf7e7` (which disabled the auto-generated `target_info` in the Prometheus exporter). Rebuild the image and reload.

**No metrics at all, even `ollie_agent_up`.** The agent failed to start the scrape listener. Check `kubectl logs -c agent` for a `scrape listen 0.0.0.0:9090: ...` error and ensure no other process is bound to `:9090` in the pod.

## Where next

- **Autoscale on captured traffic** — [`examples/hpa/`](https://github.com/gke-labs/in-cluster-observability/tree/main/examples/hpa) walks an `autoscaling/v2` HPA scaling an uninstrumented workload on its OBI-captured request rate, through the `custom.metrics.k8s.io` API the install above already registered.
- **Cluster-wide PromQL** — port-forward `deploy/ollie-query` `:9095` and hit `/api/v1/query`; the query server fans out to every agent's store and merges.

## Tearing down

```sh
kubectl delete -k k8s/
kubectl delete namespace demo
kind delete cluster --name ollie-v03
```
