---
title: Architecture
description: How the agent, OBI, the controller, and the query server fit together — the sibling-container model and the per-node store with cluster-wide fan-out.
---

Ollie is built on top of
[OpenTelemetry eBPF Instrumentation (OBI)](https://github.com/open-telemetry/opentelemetry-ebpf-instrumentation).
OBI does the eBPF capture and K8s metadata enrichment; Ollie adds the
production deployment scaffolding, declarative onboarding, a per-node
metric store with cluster-wide PromQL, and the consumer-facing APIs
(HPA custom metrics, streaming, egress).

For the full design log and the reasoning behind each choice, see the
[ADRs in the repo](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/design/decisions.md)
— especially ADR-0018 (sibling-container model), ADR-0021 (OBI native
enrichment), ADR-0024 (extensibility via wire protocols), and
ADR-0025/0026 (the v0.5 store/query shape).

## The two-container pod

Each node runs a DaemonSet pod with two containers sharing the pod's
network namespace and an `emptyDir` volume:

```
┌──────────────────────────────────────────────────────────────┐
│ DaemonSet pod (ollie-agent)                                  │
│                                                              │
│  ┌───────────────────────┐    ┌─────────────────────────┐    │
│  │ obi (sibling)         │    │ agent (this project)    │    │
│  │ otel/ebpf-instrument  │    │                         │    │
│  │                       │    │ OBI config writer       │    │
│  │ eBPF capture          │    │  ──> /etc/ollie/...     │    │
│  │  - L4 socket filter   │    │                         │    │
│  │  - L7 uprobes         │───▶│ OTLP receiver (loopback)│    │
│  │                       │    │ ├─ metric collector     │    │
│  │ K8s metadata informer │    │ │   ──> :9090/metrics   │    │
│  │  - pods, services,    │    │ ├─ embedded tsdb store  │    │
│  │    nodes, replicasets │    │ │   ──> :9091 remote-read│   │
│  └───────────────────────┘    │ ├─ span ring + CEL      │    │
│             ▲                 │ │   ──> :9092 stream    │    │
│             │ config file     │ └─ OTLP / remote-write  │    │
│             └─────────────────│      push egress        │    │
│                               └─────────────────────────┘    │
└──────────────────────────────────────────────────────────────┘
```

**OBI (sibling container)** does the kernel work: L4 TCP via an
in-kernel socket filter (dual-sided K8s attribution on every flow), L7
HTTP/1.1 via uprobes on processes matching the configured ports, and a
K8s metadata informer that attaches `k8s.*` attributes to every
captured event. It runs privileged with the eBPF capability set.

**The agent** is unprivileged and intentionally thin. It writes OBI's
config (the hook the controller drives), receives OBI's OTLP on
loopback, and serves four things per node:

- **`:9090/metrics`** — everything OBI captured plus the agent's own
  self-observability, re-emitted through a temporality-aware
  const-metric collector (cumulative totals pass through, histograms
  keep their buckets, stale series are evicted).
- **An embedded Prometheus tsdb** fed by a 1-second self-scrape of that
  same registry, exposed via Prometheus **remote read on `:9091`** —
  this is the node's slice of the cluster store.
- **A raw-OTLP span ring** with **CEL-filtered streaming on `:9092`** —
  filters compile and run at the source, so non-matching spans never
  cross the network.
- **Push egress**: raw-payload OTLP relays and Prometheus remote-write
  to operator-configured endpoints, with bounded queues that drop (and
  count) rather than backpressure capture.

The split has one real consequence: **OBI is the source of truth for
K8s identity.** The agent never runs its own informer or IP-resolution
cache — L4 flows arrive with both source and destination identity
already attached (ADR-0021, reaffirmed by ADR-0026 when the planned
identity-broadcast plane was cut as redundant).

## The cluster layer

Two Deployments complete the picture:

- **`ollie-controller`** reconciles the `TrafficMonitor` (namespaced)
  and `ClusterTrafficPolicy` (cluster-scoped) CRDs into per-node
  monitoring specs, delivered to agents over a gRPC stream. Leader
  election via Lease; agents fall back to the port-seed flag when no
  controller is present.
- **`ollie-query`** is stateless and discovers agents via the
  `ollie-agent` headless Service. It fans PromQL reads out to every
  agent's `:9091` remote-read endpoint **at the storage layer**
  (agents are secondary queriers — one dead agent degrades the answer
  and flags it, rather than failing it), evaluates centrally with the
  stock Prometheus engine, and serves:
  - **`/api/v1/query` + `/api/v1/query_range` on `:9095`**
    (bearer-token authed) — the standard PromQL HTTP API.
  - **`custom.metrics.k8s.io/v1beta1` on `:6443`** — the HPA path,
    behind the kube-aggregator with mTLS front-proxy client-cert auth.
  - **A streaming mux on `:9096`** that multiplexes the per-agent span
    streams for subscribers like `iobsctl`.

Data stays on the node that captured it until a query actually needs
it; there is no central ingest pipeline to size or shard.

## Why this shape

- **Zero application code changes.** Every label on every metric comes
  from kernel + K8s API observation.
- **One scrape URL per node**, and one query URL per cluster. Both
  speak standard protocols (Prometheus exposition, PromQL HTTP API,
  OTLP, remote-write, remote-read) — per ADR-0024, extensibility is
  wire protocols, not a Go plugin interface.
- **OBI version pinning is image-tag-based**, not Go-module-based. No
  package imports `go.opentelemetry.io/obi/*` — enforced by a test in
  `internal/archtest`. Upgrading OBI is "bump the tag, run the contract
  tests, ship."
- **Contract fixtures** recorded from the pinned OBI image freeze the
  wire behavior (types, temporality, bucket layout, attribution) so an
  OBI bump that changes semantics fails CI instead of corrupting
  metrics.

## See also

- [Getting started](/getting-started/) — concrete deploy on Kind.
- [What works today](/what-works-today/) — the verified inventory.
- [Autoscale on captured traffic](/use-cases/hpa-autoscaling/) — the
  HPA path end to end.
- [`docs/design/`](https://github.com/gke-labs/in-cluster-observability/tree/main/docs/design)
  in the repo — full design log, including per-subsystem docs.
