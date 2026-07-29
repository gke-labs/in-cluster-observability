# Roadmap

**Status:** Draft, 2026-05-17
**Owners:** TBD

This document lists what's intentionally **not** in v1, in rough priority order. Items have motivation and open questions but no dates. Once an item gets prioritized, it gets its own ADR, possibly its own design doc, and gets removed from this list.

The principle: ship a focused v1 that hits requirements §1–§7 cleanly, then expand. Every item on this list is something the requirements doc either explicitly defers or names as future-tense.

## 1. Kafka protocol parser

**Motivation.** Requirements §2.1 names Kafka as a first-class roadmap protocol. Streaming applications and event-driven microservices are a major K8s use case; Kafka traffic is currently captured at L4 only, which loses topic/partition/consumer-group semantics.

**Path.**
- OBI's 2026 roadmap (per the OBI 2026 goals blog) covers MQTT, AMQP, NATS, and Redis pub/sub. Kafka is plausible-but-not-yet-promised.
- If OBI adds Kafka: we expose it as `capture.ModuleKafka` and add the corresponding metric/span schema.
- If OBI does not add Kafka in v1.0 / v1.1: contribute upstream (default path per [ADR-0010](decisions.md#adr-0010-obi-version-pinning-and-adapter)) or implement a custom OBI `Tracer` module that lives in our adapter until upstreamed.

**Open questions.**
1. Topic / partition cardinality controls — these can explode quickly. Need templating rules baked into the default `ClusterTrafficPolicy`.
2. Consumer group offset and lag — useful but requires more than wire-level parsing (Kafka protocol responses carry it).
3. Multi-broker connections — Kafka clients hold long-lived multiplexed connections; how we attribute per-topic without false-positive cross-correlation needs design.

## 2. A2A semantic conventions

**Motivation.** A2A (Agent-to-Agent) is captured at the HTTP transport layer today, but the *interesting* attributes — agent identity, tool calls, message types, task IDs — require A2A-aware parsing.

**Path.**
- Wait for A2A on-wire conventions to stabilize.
- Add `capture.ModuleA2A` as a semantic layer over `ModuleHTTP*`, populating `a2a.*` attributes from request/response bodies.
- Likely contributes back to OBI as a protocol module, similar to OBI's GenAI instrumentation.

**Open questions.**
1. Identity propagation — how A2A names agents, and whether that maps cleanly to OTel `service.name` or needs a parallel namespace.
2. SSE-streamed responses — the parser needs to handle partial-body streaming differently from request/response pairs.
3. Tool-call attribution — likely a parallel "tool invocation" span schema, distinct from request spans.

## 3. Additional TLS library support

**Motivation.** TLS coverage in v1 is OpenSSL + Go `crypto/tls` (+ limited BoringSSL). Many workloads use rustls, NSS (Firefox, some Python stacks), Java JSSE, or specialized libraries.

**Path per library.**

| Library | Approach | Risk |
|---|---|---|
| rustls | uprobes into rustls' symbol set; OBI roadmap candidate | Moderate — rust symbol versioning has historically been a moving target |
| NSS | uprobes into NSS' SSL_Read/SSL_Write | Low — stable ABI |
| Java JSSE | bytecode instrumentation OR JNI-level uprobes; significantly harder | High — JIT'd code; needs OBI's GenAI-style auto-SDK approach |
| MbedTLS | uprobes; less common but appears in IoT/embedded | Low priority |

**Open questions.**
1. Coverage gaps versus operator expectations — operators will assume "TLS decryption" means all TLS unless we're loud about per-library scope.
2. Upstream-vs-fork on each — same policy as [ADR-0010](decisions.md#adr-0010-obi-version-pinning-and-adapter).

## 4. Multi-cluster federation

**Motivation.** Requirements §5 lists multi-cluster as a non-goal for v1. For organizations running fleets, cross-cluster topology and aggregation are eventually needed.

**Path.**
- Identity attribution across clusters — extend the topology schema with `k8s.cluster.name`.
- Cross-cluster IP resolution — needs Cilium ClusterMesh-style awareness or a federated identity cache.
- Query federation — query server learns to fan out to peer-cluster query servers.
- HPA across clusters — likely not part of v1 multi-cluster; cluster-local HPA is sufficient for most cases.

**Open questions.**
1. Whether to federate at the query plane (each cluster has its own store; query layer federates) or at the storage plane (one central store; agents from all clusters write to it). Strongly leaning federate-at-query.
2. Cross-cluster TLS / auth model.
3. Multi-cluster CRD ownership — is `TrafficMonitor` cluster-local or cluster-set-scoped?

## 5. First-party monitoring UI

**Motivation.** Requirements §2.7 made UI a soft requirement, with the assumption that Grafana + OTLP push covers most needs in v1. Eventually a first-party UI tuned to the data model (topology graph, span detail, trace-edge correlation) will matter.

**Path.**
- Single-page app embedded in the query server, served at `/ui/`.
- Built on the existing gRPC streaming + PromQL/CEL APIs — no new server surface.
- Focus: topology graph (services & edges), per-edge drill-down (latency histograms, error rates), recent spans with filtering. Not a Grafana competitor for time-series.
- For the span-detail waterfall, start from the POC-era sketch in [#54](https://github.com/gke-labs/in-cluster-observability/issues/54) (closed as premature, 2026-07-28): virtualized list of absolutely-positioned divs, plain JS first, no canvas/SVG.

**Open questions.**
1. Tech stack — React + d3 for the graph is the obvious answer. Worth a brief survey.
2. Auth model — proxy via K8s aggregation auth (rare; complex) or run behind operator's existing ingress with their auth.
3. Embedded vs separate binary — embedded is one less Service to manage but adds JS toolchain to the build.

## 6. Long-term storage adapters beyond OTLP / Prometheus

**Motivation.** OTLP-collector + Prometheus remote-write covers the common case. For organizations using ClickHouse, S3-compatible blob, or specialized observability backends (Datadog, Honeycomb, NewRelic), each could be a first-party sink.

**Path (amended by [ADR-0024](decisions.md#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157)).**
- Backends that speak OTLP or remote-write need nothing from us — they are already served by the built-in egress ([`sinks-and-extensibility.md`](sinks-and-extensibility.md)).
- Backends with proprietary ingest become either built-in exporters behind config (first-party, this repo) or external bridges consuming the streaming subscribe API (community, any language).

**Targets, rough priority:**

| Target | Why | Notes |
|---|---|---|
| ClickHouse direct | Popular for high-cardinality observability data | Native protocol; well-trodden |
| S3-compatible blob (Parquet) | Cheap long-term archival for AI-agent training corpora | Roll over closed tsdb blocks; convert to Parquet |
| Datadog | Common enterprise destination | API + metrics submission; needs token mgmt |
| Honeycomb | Native event-stream model fits our spans | OTLP works today; native is optimization |
| Splunk HEC | Enterprise logs / metrics ingest | Push HTTP API |
| BigQuery | Streaming inserts of spans for SQL analysis | Especially relevant on GCP |

## 7. Higher-cardinality histogram support

**Motivation.** Native histograms (Prometheus 2.40+) reduce storage and improve quantile accuracy at high cardinality. We default to classic histograms in v1 for compatibility.

**Path.** Add a per-`TrafficMonitor` toggle `cardinality.histogramKind: classic|native|both`. Native histograms via tsdb's native histogram support.

**Open questions.**
1. Downstream compatibility — older Prometheus scrapers don't understand native histograms.
2. Storage cost win is real but hard to project without realistic cardinality data.

## 8. eBPF program supply-chain signing

**Motivation.** Operations §10 open question. OBI's eBPF object files are loaded into the kernel at agent startup; signing + verification would close a supply-chain hole.

**Path.** Track upstream OBI signing infrastructure; add verification at adapter init.

## 9. Plugin model for non-Go sinks / filters

**Retired by [ADR-0024](decisions.md#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157) (2026-07-29).** Extensibility is wire-protocol-based (OTLP push, streaming subscribe, scrape/remote-write); there is no in-process extension point left for a plugin model to serve. Non-Go consumers were the strongest argument for WASM — they are served natively by the wire surface.

## 10. eBPF-side cardinality enforcement

**Motivation.** Currently sampling can drop events before they reach Go (via OBI's eBPF-side sampling where supported), but path templating happens in Go. If a workload generates millions of unique paths, the Go-side cost is real even though most are dropped at write.

**Path.** Push templating decisions into the eBPF program where the path is parsed. Substantial complexity; only worth it under measured load. Track via [`testing-and-benchmarks.md`](testing-and-benchmarks.md) regression data.

## 11. mTLS for controller ↔ agent

**Motivation.** Operations §4 open question. Per-node client certs would harden the controller-to-agent channel.

**Path.** cert-manager `ClusterIssuer` + per-DaemonSet-pod `Certificate` (or a single cert + SAN list; tradeoff TBD). Wait for cert rotation tooling to be operationally proven in our environment.

## 12. Sink-level filtering at core (push-down)

**Motivation.** Sinks ([`sinks-and-extensibility.md`](sinks-and-extensibility.md)) open question. Sinks today filter internally; declaring filters at registration would let core skip the dispatch for records the sink would drop anyway.

**Path.** Add an optional `Filter() string` method (CEL expression) to `PushSink`; core evaluates it before invoking `Write`. Sinks that don't implement it get every record. Backward-compatible addition.

## 13. Built-in workload-identity / SPIFFE attribution

**Motivation.** Topology ([`topology.md`](topology.md)) attributes K8s identity. Many orgs run SPIRE / SPIFFE for cryptographic workload identity; surfacing SPIFFE IDs as `spiffe.id` attributes would be welcome.

**Path.** A custom `IdentityProvider` (per [`topology.md`](topology.md) §8) that reads SPIRE's workload API. Maintained as an importable optional package, not built-in.

## 14. Continuous profiling integration

**Motivation.** Requirements §5 explicitly lists profiling as a non-goal (Parca / Pyroscope territory). But OBI's roadmap includes "integration with the OpenTelemetry eBPF profiler," and that intersects our agent — the same eBPF privileges + node placement.

**Path.** Likely never become a sink-shaped feature; more likely a sibling DaemonSet sharing some infra. Track OBI's profiler integration; revisit only if it becomes embarrassing not to.

## How this list evolves

- Items get added when requirements grow, when an OBI capability lands that's worth surfacing, or when adoption reveals a gap.
- Items get removed when they ship (and become an ADR + design doc) or when we decide they're not happening (and become an ADR documenting why).
- This file should never become a wishlist. If an item sits here for >6 months without movement, it gets reviewed for cut.
