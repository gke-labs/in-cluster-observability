---
title: What works today
description: Honest v0.5.1 inventory — what's proven end-to-end on a real cluster, what's unit-verified, what's knowingly deferred, and what's not built.
---

Ollie is at **v0.5.1**. The headline path — an HPA scaling a real
workload on eBPF-captured metrics — works end to end and is
machine-verified on every PR. This page is the inventory, tiered by
**how strongly each claim is verified**, because "merged" and "proven"
are not the same thing: an adversarial review of the merged v0.5 tree
found real defects that green CI had masked (all fixed in v0.5.1;
[ADR-0027](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/design/decisions.md#adr-0027-v051-hardening--adversarial-review-closes-real-defects-the-green-ci-self-merge-masked)
has the full story).

## Proven end-to-end on a real cluster

Every PR runs a Kind e2e suite that exercises these paths against the
real DaemonSet, the pinned OBI image, and a live workload:

- **Capture → query.** OBI captures HTTP from an uninstrumented echo
  server; the series traverses forwarder → per-node tsdb → remote-read
  fan-out → central PromQL, and
  `sum(rate(http_server_request_duration_count{...}[1m]))` returns the
  aggregate through the query server's `/api/v1/query`.
- **HPA autoscaling.** The custom-metrics adapter serves
  `custom.metrics.k8s.io/v1beta1` through the kube-aggregator — with
  mTLS front-proxy client-cert auth on `:6443` — and an
  `autoscaling/v2` HPA scales a Deployment on its captured request
  rate. The [worked example](/use-cases/hpa-autoscaling/) is these
  exact manifests.
- **OTLP export relay** of the raw captured payloads to an in-cluster
  OpenTelemetry collector, with a dead endpoint degrading to counted
  drops without touching capture.
- **Live CEL span streaming**: agent-side CEL filter → query-server
  mux → `iobsctl spans` receives a matched span from real traffic.
- **Contract fixtures** recorded from the pinned OBI image freeze the
  wire behavior Ollie depends on: metric types, cumulative temporality,
  histogram bucket layout, and dual-sided K8s attribution on L4 flows.

### What you'll see on the wire

L4 TCP flows (socket filter — every flow on the node, both identities
on one datapoint):

```
obi_network_flow_bytes{
  direction="request" | "response" | "unknown",
  k8s_src_namespace="...", k8s_src_owner_name="...", k8s_src_owner_type="...",
  k8s_dst_namespace="...", k8s_dst_owner_name="...", k8s_dst_owner_type="...",
} <bytes>
```

L7 HTTP/1.1 (uprobes on processes matching the configured ports):

```
http_server_request_duration{...k8s_pod_name, k8s_namespace_name, k8s_deployment_name, ...} <seconds>
http_server_request_body_size{...} <bytes>
http_server_response_body_size{...} <bytes>
```

Self-observability (always present, `ollie_<component>_*`):
`ollie_agent_up` is the boot signal; `ollie_capture_events_total`
bisects "is OBI feeding us" from "is downstream broken"; store, export,
and forward each have their own counters. Since v0.5.1 every self-obs
series also carries `k8s_node_name`, so per-node series stay distinct
under cluster-wide aggregation.

## Correct in code, verified at unit level

These behaviors have focused regression tests but have **not** been
demonstrated on a multi-node cluster under real failure:

- **Degraded fan-out.** An agent that dies mid-query — including
  mid-stream during chunked remote-read — is recorded as a miss and the
  response is flagged `degraded=true` with `missingNodes`, instead of
  failing the query. Simulated in tests; not yet exercised by killing
  a real pod mid-drain.
- **Auth rejection paths.** The `:6443` front-proxy middleware's
  401/403 behavior, the scrapeauth TokenReview/SAR gates, and the
  NaN/Inf → 404 guard in the custom-metrics adapter are unit-tested;
  the e2e suite proves the *positive* paths only.
- **Prometheus remote-write egress** (the e2e suite covers the OTLP
  relay; remote-write is unit-tested).
- A structural caveat behind several of these: **the e2e cluster is
  single-node**, so multi-node fan-out, cross-node merge behavior, and
  node-loss scenarios only exist as in-process simulations today.

## Knowingly shipped, tracked for v0.6

Real defects found by the v0.5 review, triaged below the fix-now line
(the complete list lives in ADR-0027):

- An HPA using `metricLabelSelector` silently gets the **unfiltered**
  aggregate.
- The documented example CEL filter tears down a span stream on any
  span lacking the referenced key.
- The span ring drops **newest** under pressure while everything
  documents drop-oldest.
- The agent memory limit (200Mi) predates the tsdb head + span ring;
  the sizing doc says 400Mi.
- Readiness gates only `:9090`, so a restarting agent can join the
  fan-out before its remote-read endpoint is up; no staleness markers,
  so a dead series freezes for up to 5 minutes instead of going stale.

## Not built yet

- **HTTP/2, gRPC, TLS uprobes** — coverage is L4 TCP + HTTP/1.1.
  A gRPC-heavy or mesh environment is largely invisible today. → v0.6.
- **TLS on internal hops** — the custom-metrics APIService still sets
  `insecureSkipTLSVerify` (client auth IS enforced via the front-proxy
  cert); agent↔query hops are bearer-token over plaintext. → v0.6.
- **Validating webhook** for the CRDs — invalid CRs are caught by CRD
  schema only. → v0.6.
- **Cardinality controls** (path templating, sampling) and
  **Pods-type HPA metrics** with label selectors. → v0.6.
- **Helm chart, operator runbook, upgrade guide, multi-arch images,
  performance budgets.** → v1.0.

## Known limitations

- `k8s.cluster.name` is empty unless you set
  `OTEL_EBPF_KUBE_CLUSTER_NAME` on the obi container. Cosmetic.
- bpffs pinning may fail on some clusters; OBI logs a warning and
  continues — L4 + L7 capture are unaffected.
- Port-based discovery (`--obi-instrument-ports`) is the coarse
  fallback; `TrafficMonitor` CRDs are the intended selector.

## See also

- [Getting started](/getting-started/) — the Kind walkthrough.
- [Autoscale on captured traffic](/use-cases/hpa-autoscaling/) — the
  HPA worked example.
- [Architecture](/architecture/) — why it's shaped this way.
- [Roadmap](/roadmap/) — what lands when.
