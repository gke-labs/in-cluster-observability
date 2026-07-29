---
title: "What works today"
linkTitle: "What works today"
weight: 20
description: "Honest v0.3 feature snapshot — what's captured, what the metric names look like, and what's not built yet."
---

Ollie is at **v0.3 dev preview**. The capture path works end-to-end on real clusters, but several pieces from the original requirements (CRD-driven onboarding, in-cluster store + PromQL, HPA custom-metrics API, AI-agent streaming, TLS uprobes, peer.k8s.* on the L7 path) are still ahead. This page is the inventory.

## What's captured

### L4 TCP — every flow on the node

OBI's network mode attaches an in-kernel socket filter and observes every TCP byte flowing through the node, regardless of which process owns it. The metric you'll see is:

```
obi_network_flow_bytes{
  direction="request" | "response" | "unknown",
  k8s_src_namespace="...", k8s_src_owner_name="...", k8s_src_owner_type="Deployment|StatefulSet|...",
  k8s_dst_namespace="...", k8s_dst_owner_name="...", k8s_dst_owner_type="...",
  host_id="...",
  obi_version="v0.10.0",
} <bytes>
```

**Dual-sided attribution is free** — both source and destination identity are on the same datapoint. You don't need to run a separate informer to resolve peer IPs; OBI does it with its built-in K8s metadata.

### L7 HTTP/1.1 — per-process attach

OBI's Application mode attaches uprobes to any process whose listening port matches the configured selector. v0.3 ships a `--obi-instrument-ports=80,443,8080,8443` seed in the DaemonSet so any process binding one of those ports is instrumented automatically. The metric families you'll see (one per HTTP request that completes):

```
http_server_request_duration{
  http_request_method="GET",
  http_response_status_code="200",
  http_route="/**",
  server_address="nginx",
  server_port="80",
  url_scheme="http",
  k8s_pod_name="nginx-...",
  k8s_namespace_name="...",
  k8s_deployment_name="...",
  k8s_replicaset_name="...",
  k8s_container_name="...",
  k8s_node_name="...",
  service_name="...",
  service_namespace="...",
} <seconds>

http_server_request_body_size{...} <bytes>
http_server_response_body_size{...} <bytes>
```

The labels come from OBI's K8s metadata informer with no additional work — `k8s.pod.name`, `k8s.namespace.name`, `k8s.deployment.name`, `k8s.statefulset.name`, `k8s.replicaset.name`, `k8s.daemonset.name`, `k8s.node.name`, `k8s.container.name`, `k8s.pod.uid`, `k8s.pod.start_time`, `k8s.cluster.name`. On GKE/EKS/AKS, cloud provider attrs (`cloud.account.id`, `cloud.region`, `cloud.platform`, etc.) come along for free too.

### Agent self-observability

Always present at `:9090/metrics`, regardless of workload activity:

```
ollie_agent_up{version="v0.3.0-dev"} 1
ollie_capture_events_total{kind="metric|span|edge", module="l4_tcp|http1|..."} <count>
ollie_capture_events_dropped_total{reason="backpressure"} <count>
ollie_capture_active_pids <count>
ollie_capture_obi_reloads_total{result="success|failure"} <count>
```

`ollie_agent_up` is the boot signal — confirms the scrape path is wired before any traffic flows. `ollie_capture_events_total{kind="metric|span"}` is the bisection point for "is OBI feeding us anything" vs "is downstream broken."

## How the data flows

```
nginx (in demo namespace)
   │
   ▼ TCP / HTTP/1.1
[node kernel]
   │ eBPF (socket filter for L4, uprobes for L7)
   ▼
obi container  ─── K8s informer attaches k8s.* + cloud.* attrs
   │
   ▼ OTLP HTTP on loopback
agent container — receives, translates, forwards through OTel SDK Meter
   │
   ▼
:9090/metrics ── Prometheus exporter ── scraped by your Prometheus
```

One scrape URL per node. No external dependency on Prometheus Operator, OTel Collector, or service mesh.

## What's NOT in v0.3

These are tracked as later milestones in upstream. Each links to its milestone:

- **No CRD-driven onboarding** — `TrafficMonitor` / `ClusterTrafficPolicy` and the controller land in [v0.4](https://github.com/gke-labs/in-cluster-observability/milestone/4). Until then, the `--obi-instrument-ports` flag on the DaemonSet seeds OBI's discovery.
- **No in-cluster metric store, no PromQL** — metrics flow through the agent's in-process Meter and are scrapable; cluster-local PromQL with `tsdb` HEAD lands in [v0.5](https://github.com/gke-labs/in-cluster-observability/milestone/5).
- **No HPA `custom.metrics.k8s.io` API** — also v0.5.
- **No AI-agent streaming with CEL filters** — also v0.5.
- **No HTTP/2, gRPC, or TLS uprobes** — [v0.6](https://github.com/gke-labs/in-cluster-observability/milestone/6) hardens protocol coverage.
- **No cardinality controls** (path templating, sampling, peer.ip collapse) — v0.6.
- **No Helm chart, no operator runbook, no upgrade guide** — [v1.0 GA](https://github.com/gke-labs/in-cluster-observability/milestone/7).

## Span / edge data

`SpanEvent` and `EdgeEvent` flow through the agent's writer goroutine and are currently *drained* — the events arrive but nothing persists them. The persistence path (an in-memory ring buffer with a queryable store) lands in v0.5. If you want spans visible today, point OBI's OTLP traces export at an external collector (the `otel_traces_export.endpoint` in `internal/obiconfig` defaults to the agent's HTTP receiver; change it to your collector).

## Known limitations

- **`k8s.cluster.name` is empty** unless you set `OTEL_EBPF_KUBE_CLUSTER_NAME` on the obi container. OBI's auto-detection doesn't pick it up on Kind or most managed clusters. Cosmetic; harmless.
- **bpffs pinning may fail** (`mkdir /sys/fs/bpf/otel: no such file or directory`) on some clusters. OBI logs a warning and continues — features depending on pinned maps (OBI's log enricher, profile correlation) are disabled, but L4 + L7 capture work fine.
- **`--obi-instrument-ports` is a coarse selector.** Anything binding port 80 gets instrumented, regardless of whether you wanted it. v0.4's `TrafficMonitor` CR will fix this with label-based workload selection.
- **No span persistence.** As noted above.

## See also

- [Getting started]({{< relref "getting-started.md" >}}) — the Kind walkthrough you can run now.
- [Architecture]({{< relref "architecture.md" >}}) — why it's shaped the way it is.
- [Roadmap]({{< relref "roadmap.md" >}}) — what's coming.
- [`docs/design/decisions.md`](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/design/decisions.md) — the full ADR log, including [ADR-0021](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/design/decisions.md#adr-0021-lean-v03--agent-re-uses-obis-native-enrichment) for why v0.3 looks the way it does.
