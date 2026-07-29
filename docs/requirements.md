# Requirements

**Status:** Agreed, 2026-05-17
**Supersedes:** the goals/coverage sections of `docs/rough_design.md` (the design sketch in that doc remains a starting point, not a commitment).

> All code currently in this repo is throwaway POC. Nothing here constrains the design; references to existing files in this document are illustrative (proof a thing is feasible), not commitments to evolve them.

## 1. Problem

Operators want network-level visibility into Kubernetes workloads (per-pod L4 and L7 metrics, plus enough signal to derive service topology) **without modifying the workloads** and, preferably, **without injecting sidecars**. The signal must be consumable by:

- **HPA** — drive autoscaling on network-derived metrics (e.g. avg requests/sec across pods of a backend service).
- **AI agents** — derive cluster topology, baseline traffic patterns, detect anomalies.
- **Ops tooling** — Prometheus, OTLP collectors, dashboards, alerting.
- **Third-party integrations** — projects that want to ship metrics to their own systems should be able to wrap us and register their own export.

The closest existing system is [Pixie](https://github.com/pixie-io/pixie); the differentiator of this project is **pluggable sinks** and a **library-first** posture.

## 2. In scope

### 2.1 Capture (eBPF agent, DaemonSet)

Capture, per pod, enough signal for L4 metrics, L7 metrics, and topology derivation.

**L4 (TCP):**
- bytes (rx/tx), connections (in/out), connection lifetime
- RTT, retransmits (from kernel TCP stats)
- **L4 timing signals** as latency proxies when L7 isn't parseable: time-to-first-response-byte, send/ack timing. (True request/response latency is an L7 concept; at L4 we surface the closest meaningful approximations and label them as such.)

**L7 — must-haves:**
- HTTP/1.1
- HTTP/2
- gRPC

**L7 — first-class but secondary:**
- **A2A (Agent2Agent).** A2A rides on HTTP + JSON-RPC + SSE, so the *transport* is captured by HTTP instrumentation. The added work is recognizing A2A semantics (agent identity, tool calls, message types) and attaching them as attributes.

**L7 — roadmap, extensibility required:**
- Kafka, and other application protocols. The capture layer must be designed so new protocol parsers can be added (ideally as plugins) without rearchitecting the agent.

**TLS:**
- Decrypted L7 via uprobes for OpenSSL, BoringSSL, Go `crypto/tls` as v1 targets; rustls / NSS / others on roadmap. This is the biggest engineering commitment in the project and is explicitly the long-tail.

**Topology metadata on every record:**
- Source K8s identity (pod, namespace, owner workload, labels) attached to every metric/span via OTel `k8s.*` resource attributes.
- Destination K8s identity resolved from peer IP (pod, service, owner workload) and attached as `peer.k8s.*` attributes. External IPs labeled as such.

### 2.2 Control plane (CRD)

A CRD is the right primitive — it's the K8s-idiomatic shape and aligns mental model with `PodMonitoring` (GKE Managed Prometheus) and `ServiceMonitor` (Prometheus Operator).

The onboarding model is **hybrid**:
- A cluster-wide default policy declares what to capture for un-annotated workloads (e.g. "L4 for everything"). The shape of this is TBD — could be a singleton cluster-scoped CR or operator config.
- A namespaced CRD (working name `TrafficMonitor`) lets workload owners opt into deeper capture (L7, TLS, additional ports) or opt out.
- The controller resolves label selectors to concrete pods/PIDs and pushes per-node config to agents.

### 2.3 Storage and query (in-cluster)

Each node runs an **in-memory, label-indexed time-series store** with:
- **Default retention: 10 minutes** (configurable). Sized to comfortably cover HPA decision windows (single-digit minutes).
- WAL or periodic snapshot to local disk for crash recovery.
- Push-down query (filter, group-by, time-window) returning in sub-second time for HPA polling.

A central query layer fans out to node-local stores and aggregates. Long-term storage is the job of external sinks (§2.4); the in-cluster store is for low-latency reads and short-term replay.

### 2.4 Sinks and extensibility — library + controller

> **Amended by [ADR-0024](design/decisions.md#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157) (2026-07-29):** the library clause below is superseded. The project ships as a **deployable system with open egress** — extensibility is delivered over the wire (OTLP push to configured endpoints, CEL-filtered streaming subscribe, Prometheus scrape/remote-write), not via an importable Go module. "Adding a sink requires no fork" stands, satisfied by network interfaces. The original text is preserved below for history; the differentiation-vs-Pixie framing is restated as *open egress*.

The project ships as **both a controller (deployable) and a library (importable Go module)**. This is a load-bearing requirement, not a nice-to-have:

- A small core (capture → in-cluster store → query) is a library.
- The default binary is a controller that registers the **built-in sinks**: OTLP push, Prometheus remote-write / scrape endpoint, `custom.metrics.k8s.io` APIService, and a streaming gRPC query API.
- Third parties import the library and **register their own sinks** (push, pull, or streaming) via a stable, documented interface. They run their own binary. We do not need to know about them.

Concrete sink interface design is design-doc territory; the requirement is that *adding a sink is a code-level registration with no fork required*. All three pull/push patterns must be supported: push (sink-initiated), pull (consumer-initiated), streaming (long-lived).

### 2.5 Standard derived metrics

The system must make the following trivially queryable, since they're what HPA and most dashboards want:

- **Requests per second** (L4 connection rate; L7 request rate per protocol).
- **Request latency** (L7; L4 proxy where L7 unavailable). Histogram with p50/p90/p99.
- **Response latency** (same).
- **Aggregations across pods of a service** — "avg req/sec across all pods of `backend-svc`" must be a one-liner query that HPA can call.

These are derived at query time from the underlying captures; the data plane should not pre-aggregate in ways that prevent re-aggregation along other dimensions.

### 2.6 Cardinality control

All three approaches in scope:

- **Templating** — HTTP path templating (`/users/123` → `/users/{id}`) on by default, with configurable rules.
- **Sampling** — per-protocol or per-workload sampling for spans and high-cardinality dimensions.
- **Server-side aggregation** — the in-cluster store supports group-by queries (per service, per deployment, per namespace) so HPA gets aggregates without pre-collapsing the raw data.

### 2.7 Monitoring UI (soft requirement)

Not a v1 hard requirement, but the system should be **designed for** a UI:
- The streaming query API and topology metadata should be rich enough that a UI can render a service graph and per-edge latency/throughput without round-tripping to the underlying agents.
- Default OTLP push should make Grafana / similar usable as the UI for v1.

## 3. Operational requirements

- Zero workload modification. No required SDK, no init container.
- No sidecars by default. Sidecars permitted only where a specific eBPF capability is genuinely unavailable from a node agent — and even then, only as opt-in.
- Cluster install is a single `kubectl apply` (or Helm) of the agent DaemonSet, the controller, and the query/sink Deployments.
- Agent runs node-privileged (CAP_BPF / CAP_SYS_ADMIN) — accepted cost.
- **Kernel / distro target:** Google Container-Optimized OS `cos-125-*` and later (kernel 6.x with BTF/CO-RE). We require GKE 1.35+ if any K8s API needs it. We do not target older / non-BTF kernels.
- **License:** Apache 2.0 for our code; all chosen dependencies must be Apache-2.0-compatible.

## 4. Consumers (informs the export contract)

- **HPA:** stable metric names, low-latency reads (single-digit seconds end-to-end), reachable via `custom.metrics.k8s.io`, supports aggregates across pods.
- **AI agents:** OTLP-shaped data, streaming + historical query, topology metadata first-class.
- **Ops dashboards:** Prometheus-scrapable surface and/or OTLP push to Grafana stack.
- **Third-party wrappers:** import as a library, register sinks, ship their own binary.

## 5. Non-goals (v1)

Calling these out so scope doesn't drift:

- **Application-internal distributed tracing.** We produce network-edge spans. App-internal spans remain the app team's job (OTel SDK).
- **Application log collection.** Fluentbit / Vector territory.
- **Continuous profiling.** Parca / Pyroscope territory.
- **Runtime security / threat detection.** Falco / Tetragon territory.
- **Multi-cluster federation in v1.** Single cluster first; multi-cluster is a roadmap concern.
- **Long-term storage.** External sinks handle this; we don't build a long-term store.

If a signal is required to derive L4 metrics, L7 metrics, or topology, it is in scope regardless of which bucket above it might also fit.

## 6. Decisions (resolved 2026-05-17)

### eBPF library: `open-telemetry/opentelemetry-ebpf-instrumentation` (formerly Beyla)

**Why:**
- **OTLP-native output** matches the pluggable-sink model — every capture is already OTLP-shaped, so external consumers need no translation layer. This is the single biggest fit factor.
- Already covers our L7 must-haves (HTTP/1.1, HTTP/2, gRPC) and ships uprobe-based TLS support for Go `crypto/tls` and OpenSSL.
- **Kubernetes-aware out of the box** — pod/service identity attachment is exactly the topology pattern we want (§2.1, §6 question 4 below). This is the Beyla feature the user cited.
- Apache 2.0, Go, under vendor-neutral OTel governance with active development.
- Has a clear protocol-module structure that supports adding new parsers (Kafka, A2A semantics).
- **Versus forking Pixie PEM:** Pixie is more battle-tested for deep L7 + TLS, but it's tightly coupled to PEM's CMU column store and PxL execution model. We'd spend our time ripping out Pixie's data plane to swap in our own — the opposite of "more flexible Pixie."
- **Versus bespoke (`cilium/ebpf` from scratch):** rebuilding HTTP/2 + gRPC + TLS-via-uprobes is multiple person-years that OTel-eBPF-instr has already done.

**Main risk:** TLS coverage isn't as deep as Pixie's. **Mitigation:** ship v1 with the libraries Beyla already supports; contribute upstream for gaps rather than fork.

### In-cluster store: Prometheus `tsdb` HEAD block (metrics) + thin ring buffer (spans/edges)

**Why:**
- Prometheus `tsdb` is Apache 2.0, Go, usable as a library (VictoriaMetrics, Thanos, Cortex all do this), and solves *exactly* our problem: in-memory label-indexed series with WAL, snapshot, and fast push-down query.
- OTLP → Prometheus mapping is stable, so OTLP-native captures land cleanly.
- Familiar operationally — PromQL semantics for aggregation are what HPA users already think in.
- **Versus DuckDB:** SQL surface and columnar query are appealing, but embedding a C++ database in a per-node DaemonSet on COS is a footgun (cgo, binary size, libc/kernel fragility). Not worth it.
- **Versus hand-roll:** we'd reinvent the wheel and end up with something Prometheus-tsdb-shaped anyway.

Spans / topology edges aren't a natural fit for tsdb — those go in a small parallel in-memory ring buffer, also WAL-backed.

### Default retention: 10 minutes in-cluster; external sinks for long-term

Covers HPA windows comfortably (HPA stabilization windows are typically 1–5 minutes). Configurable up/down. Long-term retention is the job of whatever external sink the operator wires up.

### Topology approach: Beyla-style — Kubelet PID mapping + informer-based IP resolution

- **Agent (DaemonSet)** queries the local Kubelet to map PIDs → pods on its node (fast, no central state).
- **K8s informer cache** (in-agent or in-controller) resolves remote IPs → pods/services for the peer side of every connection.
- Resource attributes follow OTel K8s semantic conventions (`k8s.pod.*`, `k8s.deployment.*`, `service.name`); peer side gets a mirrored `peer.k8s.*` namespace.
- This is the pattern Grafana Beyla pioneered — since we're using its codebase (now opentelemetry-ebpf-instrumentation), much of this is inherited.

### Kernel/distro: cos-125+ / kernel 6.x with BTF/CO-RE; GKE 1.35+ if required

Modern only. No legacy kernel compromises.

### License: Apache 2.0

For our code and all dependencies.

## 7. Still open (for design)

1. **CRD shape** — exact schema of `TrafficMonitor` and the cluster-default policy. Should align mental model with `PodMonitoring` but not necessarily its exact schema.
2. **Sink interface** — what does `RegisterSink(...)` actually look like? Function shape, lifecycle (push vs pull vs streaming), error/backpressure contract.
3. **Where does IP→pod resolution run** — in every agent (more API server load, no central failure), in the controller (less load, central dependency), or both with the agent as fallback?
4. **A2A semantic conventions** — capture transport now; semantic attribute design (agent identity, tool-call shape) is a separate v1.x conversation.
5. **WAL vs snapshot** for the in-memory store — both achieve recovery; the tradeoff is write amplification vs. recovery time.
6. **HPA aggregate API shape** — `custom.metrics.k8s.io` was prototyped in the POC, but the queryable surface (which aggregations are first-class, how `external.metrics.k8s.io` plays in) needs design.
