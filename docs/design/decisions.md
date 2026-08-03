# Architecture Decisions

**Status:** Accepted, 2026-05-17
**Owners:** TBD

This is the canonical decision log for the project. Every cross-cutting decision is recorded here as an Architecture Decision Record (ADR). Design documents under [`docs/design/`](.) implement these decisions; if you find a conflict, this file wins and the design doc should be updated.

ADRs are append-only. If a decision is superseded, add a new ADR that supersedes the old one and update the old one's status — do not edit the old text.

> **Convention:** when a design doc cites an ADR, link by ID, e.g. `[ADR-0008](decisions.md#adr-0008-query-language)`. ADR IDs are stable and never reused.

> **Future split:** once this file exceeds ~15 ADRs we will split to `docs/design/decisions/0001-*.md` following the standard ADR-tools layout.

---

## ADR-0001: eBPF data plane = OpenTelemetry eBPF Instrumentation (OBI)

**Status:** Accepted, 2026-05-17

**Context.** The project requires transparent eBPF-based capture of L4 and L7 (HTTP/1.1, HTTP/2, gRPC, A2A) and TLS-decrypted L7. Three viable paths: (a) `opentelemetry-ebpf-instrumentation` (formerly Grafana Beyla, donated to OTel), (b) fork Pixie's PEM data plane, (c) build bespoke on `cilium/ebpf`. Requirements §6 fixed the eBPF approach but left the library choice for design.

**Decision.** Use OBI as the data plane (Go package `go.opentelemetry.io/obi/pkg/ebpf`). Wrap it behind a thin adapter in `pkg/capture` (see [ADR-0010](#adr-0010-obi-version-pinning-and-adapter)).

**Consequences.**
- ✅ OTLP-shaped output matches our pluggable-sink model with no translation layer.
- ✅ HTTP/1.1, HTTP/2, gRPC, SQL, and GenAI (OpenAI/Anthropic/Gemini) instrumentation is already shipped — the GenAI surface directly serves our "AI agents calling AI agents" use case.
- ✅ Kubernetes pod identity attachment is already implemented (Beyla-pattern); we inherit topology basics.
- ✅ Apache 2.0, vendor-neutral OTel governance, active development.
- ✅ `Tracer` interface allows custom protocol modules — A2A and (eventually) Kafka can be added without forking.
- ⚠️ OBI is v0.8 (April 2026) and promises breaking changes per minor — mitigated by [ADR-0010](#adr-0010-obi-version-pinning-and-adapter).
- ⚠️ TLS coverage is less deep than Pixie's; mitigated by upstream contribution policy in [ADR-0010](#adr-0010-obi-version-pinning-and-adapter).
- ❌ Kafka protocol parser not yet in OBI — deferred to roadmap.

**Rejected alternatives.**
- *Fork Pixie PEM.* Mature and battle-tested, but tightly coupled to Pixie's CMU column store and PxL execution model. Ripping out PEM's data plane to swap in our own would consume the very engineering capacity that's supposed to differentiate us from Pixie.
- *Bespoke on `cilium/ebpf`.* Rebuilding HTTP/2 + gRPC parsing + TLS uprobes is multiple person-years OBI has already done.

---

## ADR-0002: In-cluster store = Prometheus tsdb HEAD block + parallel ring buffer

**Status:** Accepted, 2026-05-17

**Context.** Each node needs a short-retention store sized for HPA's decision window and AI-agent recent-history queries. Required: bounded memory, sub-second filter+group-by, crash recovery. Options: flat files (the POC), embedded SQL/column store (DuckDB, ClickHouse-lite), Prometheus `tsdb` HEAD as a library, hand-roll.

**Decision.** Use Prometheus `tsdb` HEAD block (`github.com/prometheus/prometheus/tsdb`) as a library for **metrics**. Use a parallel typed in-memory append-only ring buffer for **spans and topology edges** that don't fit tsdb's series model. Both back themselves to disk per [ADR-0012](#adr-0012-tsdb-block-duration-and-wal-strategy).

**Consequences.**
- ✅ Thanos and Cortex prove tsdb HEAD is usable as a library — we are not the trailblazer.
- ✅ PromQL is the operationally-familiar query language for K8s metrics; HPAs already think in it.
- ✅ Apache 2.0, pure Go, no cgo.
- ✅ OTLP → Prometheus mapping is stable and well-defined.
- ⚠️ tsdb's data model is metrics-only; spans and edges need a parallel path. Acceptable; the ring buffer is small.
- ❌ We don't get SQL-style joins. We accept this — our queries are filter-then-group-then-aggregate, which PromQL covers.

**Rejected alternatives.**
- *DuckDB.* Excellent columnar engine, but embedding a C++ database in a per-node DaemonSet on COS adds cgo, binary size, and libc/kernel fragility we don't need.
- *Hand-roll a column buffer.* We would end up shaped like tsdb HEAD anyway, having reinvented WAL and label indexing.
- *VictoriaMetrics-style custom store.* Not usable as a library — it's a server, not a kit.

---

## ADR-0003: Onboarding model = hybrid CRD

**Status:** Accepted, 2026-05-17

**Context.** Requirements §2.2 required hybrid onboarding: a cluster-wide default plus per-workload opt-in/opt-out. CRDs are the K8s-idiomatic primitive and align with `PodMonitoring` (GKE Managed Prometheus) and `ServiceMonitor` (Prometheus Operator).

**Decision.** Two CRDs:
- `TrafficMonitor` — **namespaced**, selects workloads by labels, declares protocols/ports/cardinality knobs.
- `ClusterTrafficPolicy` — **cluster-scoped**, declares the default policy applied to pods not covered by any `TrafficMonitor`. Singleton recommended but not enforced; if multiple exist, the most-specific (by ordered priority field) wins.

**Consequences.**
- ✅ Aligns mental model with existing K8s monitoring CRDs.
- ✅ Cluster operators get one-CR coverage; workload teams get per-namespace overrides.
- ⚠️ Conflict resolution (multiple `TrafficMonitor`s selecting the same pod) requires care; see [`control-plane.md`](control-plane.md) for the resolution algorithm.

---

## ADR-0004: Library + controller posture; public API in `pkg/`

**Status:** Accepted, 2026-05-17

**Context.** Requirements §2.4 made "third parties can wrap us and register their own sinks" a load-bearing requirement, not a nice-to-have.

**Decision.** Ship as both a deployable controller binary and an importable Go library. Public API in `pkg/{capture,store,query,sink,topology,controller}`; everything else under `internal/`. Embedders import only what they need; the default binary registers all built-in sinks and CRD watchers.

**Consequences.**
- ✅ Third-party integrators get a clean import boundary — `pkg/` is supported; `internal/` is fair game to change.
- ✅ The default binary remains useful for non-embedders out of the box.
- ⚠️ Public API maintenance burden — see [`public-api.md`](public-api.md) for stability tiers.

---

## ADR-0005: Topology via Kubelet PID mapping + K8s informer

**Status:** Accepted, 2026-05-17

**Context.** Requirements §2.1 required source and peer K8s identity on every record. We need (a) PID → local pod, (b) IP → remote pod/service.

**Decision.** Local PID mapping comes from the node-local Kubelet `/pods` API plus `/proc/<pid>/cgroup` cross-reference, cached. Remote IP resolution comes from a K8s informer watching Pods + Services + EndpointSlices. Custody of that informer is [ADR-0009](#adr-0009-informer-custody--hybrid). Attribute namespace follows OTel semantic conventions: `k8s.pod.*`, `k8s.namespace.*`, `k8s.deployment.*`, `service.name`, mirrored as `peer.k8s.*` for the destination side.

**Consequences.**
- ✅ Inherits Beyla-pioneered pattern; OBI already provides much of the source-side attribution.
- ✅ Standard OTel semconv → off-the-shelf dashboards and downstream consumers Just Work.
- ⚠️ Informer-related API-server load — addressed by [ADR-0009](#adr-0009-informer-custody--hybrid).

---

## ADR-0006: Kernel/distro target = COS 125+, kernel 6.x, BTF/CO-RE required

**Status:** Accepted, 2026-05-17

**Context.** Per requirements §3. OBI itself requires Linux ≥ 5.8 with BTF, amd64 or arm64 (with a documented RHEL-family 4.18+ exception we are not using). cos-125 ships kernel 6.x.

**Decision.** Floor: Google Container-Optimized OS `cos-125-*`, kernel 6.x, BTF/CO-RE required, amd64 and arm64 only. GKE 1.35+ if any K8s API surface forces it. No legacy / non-BTF kernel paths.

**Consequences.**
- ✅ All modern eBPF features available (ring buffers, BTF, CO-RE, fentry/fexit, sleepable programs).
- ✅ No CO-RE relocation fallbacks; binary size and complexity stay small.
- ❌ Will not run on older distros. Documented constraint; not a regression.

---

## ADR-0007: License = Apache 2.0

**Status:** Accepted, 2026-05-17

**Context.** Requirements §3. Per [`.ap/headers.yaml`](../../.ap/headers.yaml) Apache 2.0 headers are auto-injected on Go and shell files.

**Decision.** Apache 2.0 for our code; every direct dependency must be Apache-2.0-compatible.

**Consequences.**
- ✅ Permissive, widely adopted, compatible with OBI, Prometheus tsdb, cilium/ebpf, k8s.io/client-go.
- ⚠️ GPL-only kernel headers in eBPF C are fine — eBPF programs typically declare `LICENSE = "Dual BSD/GPL"` in the `.bpf.c` itself, which is a kernel-verifier requirement not a license on our Go.

---

## ADR-0008: Query language

**Status:** Accepted, 2026-05-17

**Context.** Multiple consumers want different query semantics. HPA wants "give me a number." AI agents want filterable streams. Ops wants Prometheus-shaped scrape. Prior plans punted this as "PromQL + custom-for-spans (CEL?)" — the Plan-agent review correctly identified that as the single biggest under-decided item.

**Decision.**
- **PromQL** is the query language for **metrics** (tsdb-backed). Standard syntax, no extensions. Consumers: HPA's `custom.metrics.k8s.io` adapter, Prometheus scrape, ops dashboards.
- **CEL** is the query language for **spans, topology edges, and anything in the ring buffer** (non-tsdb data). CEL expressions are compiled against the OTLP proto types and evaluated per record. Consumers: AI agent streaming subscribers, `otelctl`-equivalent CLI, future UI.

The two languages serve disjoint data types; consumers pick the one matching what they want. The query server's gRPC API exposes both via separate methods (`QueryMetrics` taking PromQL, `QuerySpans` / `QueryEdges` taking CEL).

**Worked examples:**
- *HPA:* PromQL `avg(rate(ollie_http_requests_total{service="backend"}[1m]))` → custom-metrics-API adapter wraps the scalar result. See [`storage-and-query.md`](storage-and-query.md#hpa-example).
- *AI agent:* CEL `span.attributes["k8s.namespace.name"] == "payments" && span.duration_ms > 100` over the spans streaming endpoint. See [`storage-and-query.md`](storage-and-query.md#ai-agent-example).
- *Prometheus scrape:* `/metrics` endpoint exposes the tsdb directly; consumers issue PromQL against whatever scrapes us.

**Consequences.**
- ✅ Each consumer gets the language fit for its data; no awkward bridging.
- ✅ PromQL is operationally familiar; CEL is the standard for in-cluster policy filtering (admission webhooks, etc.).
- ⚠️ Two languages = more docs and examples to maintain. Accepted.
- ⚠️ Cross-data-type queries ("show me HTTP error rate AND the spans that produced them") require two calls. Acceptable for v1; revisit if pain emerges.

**Rejected alternatives.**
- *PromQL only.* Spans and edges aren't time series in the labeled-counter sense; jamming them into PromQL is hostile to span-shaped queries.
- *CEL only.* Loses PromQL's rate/avg/histogram functions and the HPA adapter would have to reimplement them.
- *Custom DSL.* Unjustified novelty. Both PromQL and CEL are off-the-shelf libraries.

---

## ADR-0009: Informer custody = hybrid

**Status:** Accepted, 2026-05-17

**Context.** Remote IP → pod/service resolution requires a K8s informer cache. Three options: (a) every agent runs its own informer (no central dependency, but N× API-server load on large clusters), (b) only the controller runs it and pushes to agents (one informer total, but controller becomes a critical-path dependency for capture), (c) hybrid — controller canonical, agent local fallback. Requirements §7.3 left this open.

**Decision.** Hybrid. The controller (leader-elected) runs the canonical informer for Pods, Services, and EndpointSlices. The controller broadcasts identity deltas to agents over the same gRPC stream used to distribute `MonitoringSpec` ([`control-plane.md`](control-plane.md)). Each agent maintains a local fallback informer that is **inactive** by default and activated only if the controller's heartbeat misses ≥3 intervals (default 15s). Once the controller is reachable again, the agent's fallback informer drops back to inactive after a hold-down period (60s) to avoid flapping.

**Consequences.**
- ✅ API-server load = 1 informer set in steady state.
- ✅ Controller is not a hard data-plane dependency — agents keep monitoring through controller outages.
- ⚠️ Agent has informer code it usually doesn't run. Memory cost is real but bounded.
- ⚠️ Brief transient mismatch on controller failover (agent activates local informer; one delta might be doubly-applied). Idempotent updates handle this.

**Rejected alternatives.**
- *Agents-only.* Scales linearly with node count; on 1000-node clusters that's 1000× the watch load.
- *Controller-only.* Controller failure stops identity resolution; new pods get unattributed flow records until recovery.

---

## ADR-0010: OBI version pinning and adapter

**Status:** Accepted, 2026-05-17 — adapter-shape clause superseded by [ADR-0018](#adr-0018-obi-as-sibling-container-not-embedded-library); version-pinning principle stands but now applies to the OBI container image tag, not the Go module

**Context.** OBI is v0.8 and explicitly says minor releases may break API and behavior. We depend on it for the deepest part of the stack. Direct dependency would make every OBI bump a project-wide refactor.

**Decision.** All OBI usage lives behind `pkg/capture`, a thin adapter that exposes:
- `type Tracer interface { ... }` — our trimmed surface (start/stop, AllowPID/BlockPID, callbacks for spans/metrics/events, protocol-module enable/disable).
- A `New(cfg Config) (Tracer, error)` constructor.
- No OBI types leak through the boundary; all are translated to our `pkg/capture` types or to OTel SDK types we already depend on.

Version policy:
- Pin exactly one OBI minor at a time in `go.mod`.
- Bumping OBI happens in a **dedicated PR** that touches `pkg/capture` only.
- A **contract-test suite** in `pkg/capture/contracttest/` replays recorded eBPF events and synthetic traffic against the adapter; bumps must pass the suite unchanged. New OBI features get new contract tests before being exposed.
- Fork-vs-upstream criteria for TLS coverage gaps: default to upstream contribution. Fork only if a critical hole sits unmerged for one full OBI release cycle.

**Consequences.**
- ✅ OBI churn is one-file blast radius, not project-wide.
- ✅ Contract tests catch behavioral regressions, not just type-shape changes.
- ⚠️ Adapter is an indirection; reviewers must keep it minimal or it becomes its own forking risk.
- ⚠️ We are bottlenecked on OBI's release cadence for new protocol modules. Mitigated by ability to register custom `Tracer`s.

---

## ADR-0011: Sink interface shape

**Status:** Accepted, 2026-05-17

**Context.** Sinks need to support push (sink-initiated writes to external systems), pull (external systems scrape us), and streaming (long-lived gRPC subscribers). One unified interface fits poorly; three interfaces are clearer.

**Decision.** Three explicit interfaces in `pkg/sink`:
- `PushSink` — `Write(ctx, batch) error`. Core calls into the sink on each write batch.
- `PullSink` — `RegisterRoutes(mux)`. Core gives the sink a chance to expose HTTP handlers that pull from the store on demand.
- `StreamingSink` — `Subscribe(ctx, filter) (<-chan Event, error)`. Long-lived; core feeds events into a channel until the consumer disconnects.

All three embed `Lifecycle { Init(ctx, deps) error; Start(ctx) error; Stop(ctx) error; Name() string }`. A single struct can implement multiple interfaces (e.g. the Prometheus sink implements both `PushSink` for remote-write and `PullSink` for the scrape endpoint).

Sinks are registered via `pkg/sink.Register(s Sink)` at process start; misbehaving sinks return errors that core counts and continues — a sink cannot crash the agent.

**Consequences.**
- ✅ Each pattern has the minimal, idiomatic interface.
- ✅ Embedders implement only what makes sense for their target.
- ⚠️ Three interfaces = three docs and three example sinks. Accepted.

**Rejected alternatives.**
- *Single `Sink` interface with mode flags.* Type system can't help; mistakes surface only at runtime.
- *Channel-based only (push-via-channel).* Doesn't fit pull-style consumers like Prometheus scrape.

---

## ADR-0012: tsdb block duration and WAL strategy

**Status:** Accepted, 2026-05-17

**Context.** Prometheus tsdb's block duration is normally 2 hours; that's wrong for our 10-minute retention budget. We also need crash recovery.

**Decision.**
- Block duration: **2 minutes**. Aligned to the snapshot cadence. Retention = 5 blocks by default (10 minutes total).
- WAL: enabled, in `/var/lib/ollie/wal/`, with periodic compaction every 30 seconds.
- Snapshot strategy: tsdb's native block compaction handles it. On crash, WAL replay restores the HEAD; older closed blocks survive on disk.

**Consequences.**
- ✅ 2-min blocks match our retention granularity; configurable for operators who want longer windows.
- ✅ WAL recovery is a tsdb-native code path; we don't reinvent.
- ⚠️ Smaller blocks = more files. Acceptable at our retention sizes.

---

## ADR-0013: Module layout = new `core/` AP root

**Status:** Accepted, 2026-05-17 — migration clause amended by [ADR-0014](#adr-0014-poc-removed-early-amends-adr-0013-migration-clause); layout clause superseded by [ADR-0015](#adr-0015-collapse-core-to-repo-root-supersedes-adr-0013-layout)

**Context.** Repo already has three AP roots (`/`, `opentelemetry/`, `obs/`) per [`AGENTS.md`](../../AGENTS.md). The fresh codebase needs a home. Adding to an existing root mixes new design with disposable POC.

**Decision.** Create a new AP root at `core/` with its own `.ap/`, `images/`, `k8s/`, and Go module `github.com/gke-labs/in-cluster-observability/core`. Public packages under `core/pkg/`; private under `core/internal/`. Default binary at `core/cmd/ollie/`. The existing POC roots (`/`, `opentelemetry/`, `obs/`) stay until the new code reaches parity, then get removed in a single cleanup PR.

**Consequences.**
- ✅ Clean separation between POC and production code during the build phase.
- ✅ Reuses the project's established AP-root convention.
- ✅ Module path makes the public API surface obvious to importers.
- ⚠️ Four AP roots in the repo temporarily. Mitigated by the cleanup-PR commitment.

---

## ADR-0014: POC removed early (amends ADR-0013 migration clause)

**Status:** Accepted, 2026-05-17 (amends [ADR-0013](#adr-0013-module-layout--new-core-ap-root))

**Context.** [ADR-0013](#adr-0013-module-layout--new-core-ap-root) anticipated a transitional period where the new `core/` AP root would coexist with the three POC roots (`/`, `opentelemetry/`, `obs/`) until parity, with a single cleanup PR at v1.0 GA (issue [#123](https://github.com/gke-labs/in-cluster-observability/issues/123)). Gari pushed back on this on 2026-05-17: carrying dead code through six milestones added noise without value — the design docs already capture everything the POC taught us, and the POC is preserved on `main` via git history.

**Decision.** Remove the POC AP roots from the `rewrite` branch immediately, before v0.1 implementation begins. Issue [#123](https://github.com/gke-labs/in-cluster-observability/issues/123) was closed early.

**Consequences.**
- ✅ The `rewrite` branch reflects what we're building, not what we abandoned.
- ✅ Less grep noise during implementation.
- ✅ No "is this the new code or the old?" ambiguity.
- ⚠️ Brief "no AP roots at all" state on the `rewrite` branch until v0.1 Foundation ([#64](https://github.com/gke-labs/in-cluster-observability/issues/64)) creates the new root. CI presubmits will either no-op or fail loudly until then. Accepted on a feature branch (not `main`).
- ⚠️ The e2e harness pattern from the POC's `tests/e2e/` is gone; needs to be re-introduced when v0.1 testing work begins. Source remains on `main` for reference (`git show main:tests/e2e/harness.go`).

**Supersedes.** ADR-0013's migration clause only (*"The existing POC roots (`/`, `opentelemetry/`, `obs/`) stay until the new code reaches parity, then get removed in a single cleanup PR"*). The structural decisions in ADR-0013 are otherwise unchanged.

**Implemented in.** Commit `e5235a9` on the `rewrite` branch.

---

## ADR-0015: Collapse `core/` to repo root (supersedes ADR-0013 layout)

**Status:** Accepted, 2026-05-17 (supersedes the layout clause of [ADR-0013](#adr-0013-module-layout--new-core-ap-root))

**Context.** [ADR-0013](#adr-0013-module-layout--new-core-ap-root) placed the new code under a `core/` AP root subdirectory. The rationale was to isolate the rewrite from the three POC AP roots that would coexist temporarily. With the POC removed early ([ADR-0014](#adr-0014-poc-removed-early-amends-adr-0013-migration-clause)) and only one AP root planned going forward, the original rationale no longer applies.

**Decision.** Put the new code at the **repo root**, not under `core/`:

- Single Go module `github.com/gke-labs/in-cluster-observability` (no `/core` suffix).
- Single AP root at the repo root: `/.ap/`.
- Public packages under `pkg/{capture,store,query,sink,topology,controller,schema,obsapi}`; private under `internal/`.
- Default binary at `cmd/ollie/`.
- Manifests at `k8s/`, images at `images/`, tests at `tests/`, protos at `proto/`, dashboards at `dashboards/`, Helm chart at `helm/`.

All path references in earlier ADRs and design docs are updated to drop the `core/` prefix. ADR-0013's original text is preserved as the historical decision record; this ADR documents the change.

**Consequences.**
- ✅ Idiomatic Go layout — packages at `pkg/capture` instead of `pkg/capture`.
- ✅ Module path matches the project name with no awkward suffix.
- ✅ Removes the only reason for `core/`, which was POC coexistence.
- ⚠️ Earlier design docs and the seed issues (~60) were authored with `core/` paths and were swept to drop the prefix.
- ⚠️ Loses optionality for future additional AP roots (e.g. a separate CLI repo or experimental subproject). Acceptable — if that becomes needed, we add a new AP root then.

**Supersedes.** ADR-0013's layout clause (`core/` subdirectory). ADR-0013's choices of `pkg/` vs `internal/`, public-API stability, and default-binary structure all stand.

**Implemented in.** Sed sweep across design docs, AGENTS.md, and seed issue bodies on 2026-05-17.

---

## ADR-0016: OBI import boundary enforced via Go test

**Status:** Accepted, 2026-05-17

**Context.** [ADR-0010](#adr-0010-obi-version-pinning-and-adapter) quarantines all OBI usage behind `pkg/capture` so OBI's v0 churn has one-file blast radius. That decision is only useful if the boundary is mechanically enforced — a code-review-only rule will be violated within a release. The design doc says "a linter / build rule fails if any other package imports go.opentelemetry.io/obi/* directly," but leaves the mechanism open.

**Decision.** Enforce the boundary as a **Go test** in `internal/archtest`. The test parses every `.go` file in the module (stdlib `go/parser`, imports-only) and fails if any file outside `pkg/capture/` imports a path under `go.opentelemetry.io/obi`. It runs as part of `go test ./...` and the CI `ap-test` presubmit; no separate tool or build-time hook is needed.

**Consequences.**
- ✅ No new dependencies (stdlib `go/parser` only).
- ✅ Fits the existing `go test` / `ap test` developer workflow; no new lint tool to install or wire into editors.
- ✅ Easy to extend with sibling architectural assertions (one Go file per invariant).
- ✅ Fast (low-hundreds-of-ms even at full repo size since we use `parser.ImportsOnly`).
- ⚠️ Runs at `go test` time, not at compile time. A developer who runs `go build` without `go test` can land a violation locally. CI catches it before merge.
- ⚠️ The test hardcodes `pkg/capture` as the only allowed importer. If a future ADR moves the adapter, this test must move with it.

**Rejected alternatives.**
- *Custom `go vet` analyzer.* More complex, needs separate distribution to developers; benefit is compile-time check, but CI catches this anyway.
- *Build tag / `//go:build` trick.* Doesn't compose well with the rest of the codebase; non-obvious failure mode.
- *Convention only, enforced by review.* Will be violated. The whole point of [ADR-0010](#adr-0010-obi-version-pinning-and-adapter) is mechanical isolation.

**Implemented in.** `internal/archtest/import_boundary_test.go` (commit landing v0.1).

---

## ADR-0017: v0.2 Capture MVP implementation decisions

**Status:** Accepted, 2026-05-17 — sub-decisions 17.1, 17.4, 17.5 amended by [ADR-0018](#adr-0018-obi-as-sibling-container-not-embedded-library) (OBI runs as a sibling container, not an embedded Go library); 17.2 and 17.3 stand

**Context.** Five implementation-time decisions for v0.2 (Capture MVP, issues [#70](https://github.com/gke-labs/in-cluster-observability/issues/70)–[#77](https://github.com/gke-labs/in-cluster-observability/issues/77)) that aren't large enough to merit individual ADRs but should be captured before work begins. Filed as one ADR to keep the log tight; v0.2 implementation PRs reference this ADR for justification per sub-decision.

### 17.1 OBI version pin

**Decision.** v0.2's first OBI integration PR pins **the latest stable OBI v0.x at time of that commit** in `go.mod`. Per [ADR-0010](#adr-0010-obi-version-pinning-and-adapter), one minor at a time; subsequent bumps live in dedicated PRs with the contract-test suite green.

### 17.2 Self-observability metrics library = OpenTelemetry SDK

**Decision.** Use the **OpenTelemetry metrics SDK** (`go.opentelemetry.io/otel/metric` + `go.opentelemetry.io/otel/sdk/metric`) for self-observability across the agent, controller, and query server. The Prometheus scrape sink (v0.3 [#82](https://github.com/gke-labs/in-cluster-observability/issues/82)) wraps the SDK via `go.opentelemetry.io/otel/exporters/prometheus`, exposing `/metrics` in Prometheus text format with no functional loss for operators with existing Prometheus deployments.

**Rationale.**
- OBI emits OTel-shaped data; consistent metrics vocabulary across data plane and self-observability path. No "two metric SDKs to reason about."
- Single SDK powers OTLP push, Prometheus scrape, and any future OTel Collector receiver — one source of truth.
- Industry direction: OTel metrics SDK reached 1.0 stable; OBI / Beyla / Grafana ecosystem is OTel-native; Prometheus itself is converging toward OTLP ingestion. Picking `prometheus/client_golang` today reads as a legacy decision in two years.

**Rejected alternative.** `github.com/prometheus/client_golang` directly. Pros: smaller dep, ~5 lines to instantiate a counter. Cons: forks the project's metrics vocabulary, agent self-obs would be Prometheus-shaped while data-plane outputs are OTel-shaped, and any future OTLP push of self-obs metrics needs a translation shim.

**Consequences.** Three OTel modules become the first non-stdlib deps in `go.mod`. ~20–30 lines of boilerplate per component to instantiate `MeterProvider` / `Reader` / `Exporter`, paid once. Operators see no change at the wire — `/metrics` still serves Prometheus text format.

### 17.3 Debug HTTP endpoint = loopback-only, default off

**Decision.** The agent's debug HTTP endpoint ([#75](https://github.com/gke-labs/in-cluster-observability/issues/75)) binds **`127.0.0.1:9099`** only, behind a **`--debug-endpoint`** flag that **defaults to off**. No authentication required because the listener is loopback-only — access requires `kubectl exec` into the agent pod's network namespace.

**Rationale.** Avoids designing an auth story for a v0.2-only convenience surface. The endpoint exists to drive `AllowPID` / `BlockPID` manually until the controller (v0.4) takes over CRD-driven monitoring.

**Consequences.** Operators who want to drive the debug endpoint from another pod or node must wait for the controller. Acceptable for v0.2 — its audience is developers smoke-testing the capture path, not operators.

### 17.4 Strip OBI's built-in Kubernetes attribution

**Decision.** Disable OBI's native K8s identity attachment for v0.2 Events. All Kubernetes attribution lands via our own `pkg/topology` resolver starting in v0.3 ([#80](https://github.com/gke-labs/in-cluster-observability/issues/80), [#81](https://github.com/gke-labs/in-cluster-observability/issues/81)).

**Rationale.** Two sources of K8s metadata on the same Event creates "which is canonical?" ambiguity and forces our enricher to know what OBI already did to avoid double-decoration. [`docs/design/topology.md`](topology.md) assumes single ownership by `pkg/topology`; honoring that from v0.2 simplifies v0.3.

**Consequences.** v0.2 Events carry no `k8s.*` attributes (only PID + protocol + payload-specific fields per [17.5](#175-v02-metricspan-field-set--minimal-http-focused)). v0.3's enricher populates `k8s.*` from `pkg/topology`. Smoke tests in v0.2 show raw PID-tagged events; K8s-attributed events arrive with v0.3.

**Rejected alternative.** Let OBI attach its K8s attrs and have the enricher overwrite. Works but invites bugs when the two disagree.

### 17.5 v0.2 metric/span field set = minimal HTTP-focused

**Decision.** `MetricEvent` and `SpanEvent` in v0.2 carry only the fields needed to demo HTTP request count + duration via the debug log endpoint: `{path, method, status, duration_ns}` for HTTP; `{bytes_rx, bytes_tx, conns, rtt_ns}` for L4. Full OTel-shaped payloads (attributes maps, full semantic-convention coverage) arrive with v0.3 when there's a store to land in.

**Rationale.** Avoid designing the field set twice. v0.2 has no store and no enricher; sinking events to a debug log only requires the demo fields. v0.3 ([#83](https://github.com/gke-labs/in-cluster-observability/issues/83)) codifies the full schema via `pkg/schema`; the `MetricEvent` / `SpanEvent` types fill in then.

**Consequences.** `pkg/sink.Metric`, `Span`, `Edge` stay empty stubs through v0.2 (already true post-v0.1). Embedders in v0.2 should treat them as "shape only" — no real data flows. Path field is captured raw at this point (templating arrives in v0.6 [#108](https://github.com/gke-labs/in-cluster-observability/issues/108)); v0.2 cardinality is bounded only by the test workload's path set, which is acceptable for the milestone's local-test audience.

---

**Implemented in.** v0.2 milestone work ([#70](https://github.com/gke-labs/in-cluster-observability/issues/70)–[#77](https://github.com/gke-labs/in-cluster-observability/issues/77)). Each issue's PR references this ADR for the relevant sub-decision.

---

## ADR-0018: OBI as sibling container, not embedded library

**Status:** Accepted, 2026-05-17 — supersedes [ADR-0010](#adr-0010-obi-version-pinning-and-adapter)'s adapter-shape clause; amends [ADR-0017](#adr-0017-v02-capture-mvp-implementation-decisions) sub-decisions 17.1, 17.4, 17.5

**Context.** ADR-0010 assumed OBI's `pkg/ebpf` was suitable for embedded library use, anticipating a thin adapter (~600–800 LoC) wrapping its Go API. Probing OBI v0.9.0 at the start of v0.2 implementation revealed reality:

- `pkg/ebpf.NewProcessTracer(tracerType, []Tracer, *obi.Config, imetrics.Reporter) *ProcessTracer` is a low-level building-block API. Embedders supply protocol-module `Tracer` implementations themselves (each `Tracer` is a ~15-method interface mixing `PIDsAccounter`, `KprobesTracer`, `GoProbes`, `UProbes`, `SocketFilters`, `SockMsgs`, `SockOps`, `Iters`, `Tracing`, instrumented-lib bookkeeping, offset registration, and event-context wiring) plus an `EBPFEventContext`, an `*obi.Config`, and a `msg.Queue[[]request.Span]` for output.
- OBI's higher-level orchestrator (`pkg/appolly/instrumenter.go`) is undocumented and uses OBI-internal types.
- OBI's README and supported deployment model is **as a standalone binary that emits OTLP**, not as a Go library to compose with.
- The actual lift to wrap `pkg/ebpf` is multiple person-weeks per protocol with the adapter pinned to OBI's internal contracts — exactly the churn ADR-0010 sought to insulate against.

**Decision.** Run OBI as a **sibling container** in the same agent DaemonSet pod. OBI emits OTLP to `127.0.0.1:4317`; our agent is an **OTLP receiver** that runs the enrichment, store, and sinks. The agent does **not** import OBI as a Go dependency.

Deployment shape:

| Container | Image | Privileges | Purpose |
|---|---|---|---|
| `obi` | `ghcr.io/open-telemetry/obi:<pinned-tag>` | `CAP_BPF` + `CAP_PERFMON` + `CAP_NET_ADMIN` + `hostPID` + host mounts | eBPF capture; emits OTLP to localhost |
| `agent` | `ollie:<our-tag>` | **unprivileged**; `runAsNonRoot: true` | OTLP receiver → enricher → store → sinks; OBI config writer |

Both containers share the pod network namespace (loopback for OTLP) and the pod lifecycle. OBI's config is mounted from a ConfigMap that the controller (v0.4) writes; on `MonitoringSpec` changes the controller updates the ConfigMap and either signals reload or relies on OBI's config watch.

The `pkg/capture.Manager` interface from v0.1 stays — its implementation pivots from "Go API caller" to "OTLP receiver + OBI lifecycle controller (config writer + reload signaler)."

**Consequences.**

- ✅ Adapter complexity drops dramatically. No `Tracer` interface implementations; no wiring through OBI's internal API surface; no `EBPFEventContext` plumbing.
- ✅ OBI version churn is decoupled from our build. Image-tag bump = test in CI, ship. No `pkg/capture` rewrites per OBI minor.
- ✅ Security posture improves: only the OBI container runs privileged. The agent container has no `CAP_BPF`, no `hostPID`, no host mounts. The threat model in [`operations.md` §4](operations.md) simplifies meaningfully.
- ✅ Aligns with how OBI is designed and supported by upstream. We consume the tool the way its maintainers intended.
- ✅ Matches the broader OTel ecosystem pattern (Beyla-as-sidecar, OTel Collector composition, Jaeger agent).
- ⚠️ Two images in the agent pod. Resource overhead is two processes instead of one; OBI's footprint is what it always was.
- ⚠️ Custom protocol modules (e.g. A2A semantic layer, Kafka before OBI supports it) become **upstream contributions** rather than additions to our adapter. Less control, but better fit for the ecosystem and easier to maintain.
- ⚠️ Per-PID enable/disable becomes **config-driven** (rewrite OBI's discovery config + signal reload), not direct API call. Slightly less crisp but operationally fine and matches OBI's own model.

**Supersedes.** ADR-0010's "thin adapter wrapping OBI's library API" framing. ADR-0010's "one version pinned at a time, dedicated bump PRs with contract tests green" principle stands but applies to the OBI **image tag**, not the Go module.

**Amends ADR-0017.**

- **17.1 OBI version pin:** now pin the OBI **image tag** in our manifest/Helm values, not the Go module in `go.mod`. The Go module dep was probe-only and is removed.
- **17.4 Strip OBI's K8s attribution:** now via OBI **configuration** (disable Kubernetes metadata decoration in OBI's config), not in our adapter.
- **17.5 Minimal field set:** OBI emits OTLP. Our v0.2 work is OTLP→`capture.Event` translation — much simpler than the originally anticipated OBI-event→`Event` translation.

**Sub-decisions 17.2 (OTel metrics SDK) and 17.3 (debug HTTP endpoint loopback-only) stand unchanged.**

**Rejected alternatives.**

- *Deep embed via `pkg/ebpf` directly.* Multi-week per protocol, fragile against OBI minor versions, no documented support path. Already costly at v0.2; intractable across v0.6's TLS coverage and v1.0's full protocol suite.
- *Vendor OBI's internal pipeline (`pkg/appolly/instrumenter.go`).* Undocumented and OBI-internal. Would force us to track every internal change with no upstream guarantees.
- *Switch eBPF library.* Forking Pixie PEM hits the same embed-vs-sibling dilemma with worse coupling. Bespoke `cilium/ebpf` is a multi-year reinvention of what OBI already does. No clearly better alternative for our requirements.

**Implementation impact on v0.2.** Issues [#70](https://github.com/gke-labs/in-cluster-observability/issues/70)–[#77](https://github.com/gke-labs/in-cluster-observability/issues/77) stay valid but their implementations pivot:

- **#70 Manager Start/Stop/EnableModule:** Start launches an OTLP receiver on `127.0.0.1:4317` (gRPC) and `:4318` (HTTP); module toggles update the OBI config and signal reload.
- **#71 AllowPID/BlockPID:** writes per-PID enable/disable into OBI's discovery config (a YAML file mounted from a ConfigMap).
- **#72 L4 TCP translation, #73 HTTP/1.1 translation:** translate OTLP `ExportMetricsServiceRequest` / `ExportTraceServiceRequest` to `capture.Event`. Field-set commitment from 17.5 stands.
- **#74 Contract tests:** fixtures are now recorded OTLP request bodies (binary protobuf), not eBPF event recordings. Simpler to capture; can be generated by replaying canary traffic through a real OBI.
- **#75 Debug HTTP endpoint:** unchanged.
- **#76 Self-obs metrics:** unchanged.
- **#77 Panic recovery:** the agent-side panic recovery shrinks (no eBPF reader goroutine to wrap). What's added is *OBI container health monitoring* — if the OBI sidecar dies repeatedly, our agent emits `incluster_obs_capture_obi_restarts_total` and surfaces the issue. Container restart itself is k8s's job.

**Implementation status.** Approved for v0.2 implementation on 2026-05-17. v0.2 work proceeds under the sibling-container model as documented above; the design is settled and not subject to re-litigation absent new information. Implementation is tracked across milestone v0.2 issues [#70](https://github.com/gke-labs/in-cluster-observability/issues/70)–[#77](https://github.com/gke-labs/in-cluster-observability/issues/77), whose bodies were updated on the same date to reflect this model. The first v0.2 implementation PR will include the manifest update adding the OBI container to the agent DaemonSet.

---

## ADR-0019: v0.2 Capture MVP implementation-time decisions

**Status:** Accepted, 2026-05-17

**Context.** v0.2 implementation under the sibling-container model ([ADR-0018](#adr-0018-obi-as-sibling-container-not-embedded-library)) surfaced several small choices that aren't large enough to warrant individual ADRs but should be recorded for future readers. Filed here per the user's instruction to capture decisions in the append-only log.

### 19.1 OTLP receiver = direct gRPC + stdlib net/http

**Decision.** The OTLP receivers in `internal/otlpreceiver` use `google.golang.org/grpc` directly (registering the standard OTLP collector service stubs from `go.opentelemetry.io/proto/otlp`) plus stdlib `net/http` for the HTTP surface. We do NOT pull in the OpenTelemetry Collector SDK receiver components.

**Why.** Smaller dep footprint (no Collector framework + its processors/exporters), simpler control flow, and our receive surface is narrow (three Export RPCs). The Collector SDK would help if we needed pluggable processing/export pipelines in the receiver itself; we don't.

### 19.2 OBI config writer YAML library = gopkg.in/yaml.v3

**Decision.** `internal/obiconfig` marshals via `gopkg.in/yaml.v3`. Rejected: `sigs.k8s.io/yaml` (wraps yaml.v2 with JSON-compatible tags — overkill since OBI's config schema doesn't need JSON round-tripping), encoding/json (OBI's config is YAML, not JSON).

### 19.3 OBI reload mechanism = file-watch (write-then-rename)

**Decision.** Per [`obi-integration.md`](obi-integration.md) §3.4 the agent writes OBI's config atomically (temp file + rename) and relies on OBI's built-in file watcher to pick it up. SIGHUP is the fallback if OBI's file watcher proves unreliable; not used in v0.2.

**Why.** Avoids requiring `shareProcessNamespace: true` on the pod (which has security implications). The atomic-rename approach is the standard pattern for config-file-watching daemons.

### 19.4 Reload coalescer debounce = 500 ms

**Decision.** `bridgeManager`'s reload coalescer debounces rapid AllowPID/BlockPID/EnableModule/DisableModule calls over a 500 ms quiet window before invoking the OBI config writer. Matches the value documented in `obi-integration.md` §5.

**Why.** A rollout that churns dozens of PIDs per second would otherwise produce dozens of OBI config rewrites + reload signals. 500 ms is long enough to coalesce a workload restart and short enough that operators don't perceive lag.

### 19.5 OBI image tag pinned = v0.9.0

**Decision.** The v0.2 DaemonSet manifest pins `ghcr.io/open-telemetry/obi:v0.9.0`. Future bumps follow the [ADR-0010](#adr-0010-obi-version-pinning-and-adapter) / [ADR-0018](#adr-0018-obi-as-sibling-container-not-embedded-library) single-bump-PR policy.

### 19.6 Contract test fixtures = synthetic seed first, real OBI second

**Decision.** v0.2 ships with synthetic seed fixtures (`l4-basic`, `http1-basic`) under `tests/contract/obi/testdata/translation/`, generated by `go test ./tests/contract/obi -seed -update`. The recipe for regenerating these from a real OBI sidecar (and replacing the synthetic ones) lives in `tests/contract/obi/REGENERATE.md`.

**Why.** Real-OBI recording infrastructure (Kind + OTel-collector-as-recorder + canary workload) is a multi-hour build that's not on v0.2's critical path. Synthetic seeds give CI a working contract today; real recordings displace them on the first OBI image bump.

### 19.7 pkg/capture exports TranslateMetrics / TranslateTraces

**Decision.** The OTLP→Event translators are exported from `pkg/capture` (Stability: Experimental). Embedders can call them directly for ad-hoc OTLP translation; the contract-test harness uses this to bypass the OTLP-receiver roundtrip.

**Why.** The alternative — keeping them unexported and reaching in via reflection from the contract tests — fought Go's unexported-field access rules and produced fragile test code. Exporting is cleaner, and the functions are useful enough that an embedder might want them.

### 19.8 Stale-config detection deferred

**Decision.** The "OBI rejects our config; agent backs off" detection mentioned in [`obi-integration.md`](obi-integration.md) §9 is **deferred to v0.3**. v0.2 trusts that the config it writes is valid (the YAML schema is small and exercised in unit tests); operators rely on OBI's startup logs if a config is rejected.

**Why.** Implementing reliable detection requires either reading OBI's stderr (cross-container; needs Downward API or log scraping) or polling OBI's `/healthz`. Neither is on the v0.2 critical path; v0.3's controller work is a more natural home.

---

**Implemented in.** v0.2 milestone work; each commit references the relevant sub-decision when applicable. ADR is non-superseding — these are all forward-compatible implementation choices.

---

## ADR-0020: v0.3 Storage MVP implementation decisions

**Status:** Superseded in part by [ADR-0021](#adr-0021-lean-v03--agent-re-uses-obis-native-enrichment). **Reconstructed stub, 2026-07-28** — the original text of this ADR was never committed to this file; ADR-0021 referenced it on 2026-05-18 but the entry was lost. This stub restores the record from ADR-0021's per-clause disposition so the numbering and cross-references resolve. ADR-0021 is authoritative for every clause.

**Context.** Implementation-time decisions for the original v0.3 "Storage MVP" (before the lean pivot). Sub-decision titles, with their fate per ADR-0021:

- **20.1** WAL = reuse `prometheus/tsdb/wal` — moot (no metric store in v0.3); revisits in v0.5.
- **20.2** Kubelet client = plain `net/http` — withdrawn (no pidcache).
- **20.3** Enricher = dedicated `internal/enricher` — withdrawn (no enricher).
- **20.4** Scrape sink on `:9090` — restated: `:9090` is the OTel SDK Prometheus exporter on the agent.
- **20.5** `pkg/schema` = constants-only — stands.
- **20.6** In-memory mode = empty `WALPath` — moot; v0.5 reconsiders.
- **20.7** tsdb HEAD + WAL deferred — moot; v0.5's plan unchanged.

---

## ADR-0021: Lean v0.3 — agent re-uses OBI's native enrichment

**Status:** Accepted, 2026-05-18 — supersedes [ADR-0017.4](#174-strip-obis-built-in-kubernetes-attribution); supersedes the storage / enricher / scrape-sink sub-decisions in [ADR-0020](#adr-0020-v03-storage-mvp-implementation-decisions); the WAL deferral in ADR-0020.7 is moot (no `pkg/store` metric path to back).

**Context.** The first cut at v0.3 ("Storage MVP") landed `internal/pidcache`, `internal/enricher`, `pkg/sink/promscrape`, `pkg/store.MetricStore`, and a metric-name translator that rewrote OBI's metric names to an `ollie_*` prefix. End-to-end smoke testing in Kind exposed two things:

1. OBI v0.9 already ships everything those packages reimplemented: a K8s informer (`OTEL_EBPF_KUBE_METADATA_ENABLE`) that attaches a *superset* of the labels our enricher attached (`k8s.namespace.name`, `k8s.deployment.name`, `k8s.statefulset.name`, `k8s.replicaset.name`, `k8s.daemonset.name`, `k8s.node.name`, `k8s.pod.name`, `k8s.container.name`, `k8s.pod.uid`, `k8s.pod.start_time`, `k8s.cluster.name`), a Prometheus exporter, and OTel-standard metric names. Our pidcache was a Kubelet-only reimplementation of OBI's informer; our enricher attached a strict subset; our promscrape sink wrapped an exporter OBI already ships; our metric-name rewrite added nothing.
2. The agent had never actually been driving OBI's discovery in production because `--config=...` is not OBI's flag — it reads `OTEL_EBPF_CONFIG_PATH` instead, and our manifest was passing the wrong knob. The "Storage MVP" was capturing L4 (which uses an in-kernel socket filter and ignores discovery) and silently producing zero L7 events.

The build-it-ourselves direction was therefore both unnecessary (OBI does it) and incorrect (we'd been masking the fact that the file we wrote was never being read). The genuinely-unique value of this project lives one layer up: declarative CRD-driven onboarding (v0.4), in-cluster storage + query plane (v0.5), HPA custom-metrics API (v0.5), AI-agent CEL streaming (v0.5), and dual-sided edge identity (v0.5). v0.3 should stop replicating OBI and start being the thin agent + production-deployment milestone those layers sit on.

**Decision.** Lean v0.3:

- **OBI does enrichment.** OBI's K8s informer is on (`OTEL_EBPF_KUBE_METADATA_ENABLE=autodetect`, `attributes.kubernetes.enable: true`). The agent does not run its own informer or PID cache.
- **OBI's metric/span names are exported verbatim.** The OTLP→Event translator passes names through; no `ollie_*` prefix rewrite.
- **Single scrape URL through the agent.** OBI exports OTLP to the agent's loopback receiver; the agent re-exposes the metric stream via the OTel SDK's Prometheus exporter on `:9090`. The OBI process never exposes a port of its own.
- **No pidcache, enricher, MetricStore wrapper, or dedicated promscrape package.** `internal/pidcache`, `internal/enricher`, `pkg/sink/promscrape`, and `pkg/store/metric_store.go` are deleted.
- **The OTLP receiver stays.** It is the hook point for v0.4 (controller-driven per-event filtering) and v0.5 (storage). v0.3 keeps it as a near-passthrough; this is the deliberate carve-out from the simplification.
- **`pkg/store.SpanEdgeStore` + `Ring[T]` stay.** v0.5 needs them for the span/edge query path; they have no equivalent in OBI.
- **`pkg/schema` stays** as the source of label-key / bucket constants for the future store. The metric-name constants are no longer used at write time (OBI's names go through) but remain as the canonical schema reference.
- **Kubernetes manifests gain the RBAC OBI's informer requires:** ServiceAccount + ClusterRole granting `list,watch` on pods/services/nodes/replicasets, ClusterRoleBinding.
- **`--obi-instrument-ports` flag on the agent** seeds OBI's `discovery.instrument` with one synthetic entry until the v0.4 controller drives discovery from `TrafficMonitor` CRs. Without this (or env-var equivalents OBI also accepts), OBI's Application mode has nothing to attach to.

**Consequences.**

- v0.3 ships in ~30% of the code the original "Storage MVP" did. Most of the deletion is in `internal/` (private), so no public API breakage.
- The agent's job description sharpens to: *write OBI's config + receive its OTLP + expose Prometheus + reserve a hook point for future filtering/storage.* That's the actual project thesis.
- v0.3's hand-off doc honestly says "OBI does enrichment; this milestone is the production deployment + agent scaffolding." No more pretending the agent does meaningful storage.
- v0.4 (controller) work is unaffected — the controller targets `bridgeManager.AllowPID` exactly as before, only now AllowPID's emitted `Instrument` entries land in a config file OBI actually reads.
- v0.5 (store + query + HPA + streaming) is unaffected in scope but the runway is cleaner — no half-built `MetricStore` to retrofit; `SpanEdgeStore` is already in place.
- [ADR-0017.4](#174-strip-obis-built-in-kubernetes-attribution)'s rationale ("two sources of K8s metadata create ambiguity") is preserved by going the *other* direction: OBI is the single source, our agent has none. The ambiguity is resolved by absence.
- [ADR-0020](#adr-0020-v03-storage-mvp-implementation-decisions)'s sub-decisions stand or fall as follows:
  - 20.1 (WAL = tsdb/wal reuse) — moot (no metric store in v0.3); revisits in v0.5 with the real tsdb HEAD wiring.
  - 20.2 (Kubelet = plain net/http) — withdrawn (no pidcache).
  - 20.3 (enricher = dedicated `internal/enricher`) — withdrawn (no enricher).
  - 20.4 (scrape sink = `:9090`) — restated: `:9090` is the OTel SDK Prometheus exporter on the agent, not a dedicated `pkg/sink/promscrape` package.
  - 20.5 (schema = constants-only) — stands; usage shifts to label-key references rather than metric-name writes.
  - 20.6 (in-memory mode = empty `WALPath`) — moot (no store in v0.3); v0.5 reconsiders.
  - 20.7 (tsdb HEAD + WAL deferred) — moot; v0.5's plan is unchanged.

**Rejected alternatives.**

- *Keep the "Storage MVP" framing and fix the bugs.* Would have shipped working code, but it'd still be replicating OBI. The right time to notice was now, not after we'd built three more milestones on top.
- *Skip the OTLP receiver and point OBI's Prometheus exporter directly at external scrapers.* Cleanest possible v0.3, but eliminates the hook point v0.4/v0.5 need. The OTLP receiver is the cheapest forward-compat we can pay today.
- *Abandon v0.1 and v0.2 PRs and start the architecture fresh.* Both milestones are correct foundations under either framing (module layout, package skeleton, OBI sibling-container model, OTLP receiver scaffolding, config writer + AllowPID/BlockPID). Only v0.3 needs the rewrite; the prior milestones do not.

---

## ADR-0022: v0.4 Control Plane implementation decisions

**Status:** Accepted, 2026-05-18 — refines [ADR-0003](#adr-0003-onboarding-model--hybrid-crd) (CRD onboarding model is unchanged; this ADR pins the implementation details); narrows [ADR-0009](#adr-0009-informer-custody--hybrid) (identity broadcasting cut from v0.4 per ADR-0021, may reopen in v0.5+).

**Context.** v0.4 is the Control Plane MVP — the milestone that replaces v0.3's `--obi-instrument-ports` smoke flag with declarative `TrafficMonitor` + `ClusterTrafficPolicy` CRDs reconciled by a Deployment-shaped controller that streams `MonitoringSpec` deltas to agents. `docs/design/control-plane.md` has the architecture; this ADR records the implementation choices made before code started landing so future readers can find the rationale in one place.

### 22.1 API group = `ollie.gke-labs.dev` (was `obs.gke-labs.dev`)

**Decision.** All v0.4 CRDs live at API group **`ollie.gke-labs.dev`**, version **`v1alpha1`** at landing. Supersedes the `obs.gke-labs.dev` placeholder that appeared in early drafts of `control-plane.md`.

**Why.** Consistent with the project name "Ollie" and with the binary / image / metric prefix `ollie`. `kubectl get trafficmonitors.ollie.gke-labs.dev` reads naturally; `obs.gke-labs.dev` reads as "what is obs" to a first-time operator. Choosing now (before any CRD types exist on disk) avoids an API-version migration later.

**Consequences.** Every CRD-touching artifact lands at this group: Go `// +groupName=ollie.gke-labs.dev`, CRD `metadata.name: <resource>.ollie.gke-labs.dev`, RBAC rules `apiGroups: [ollie.gke-labs.dev]`, kubectl invocations in docs.

### 22.2 Controller framework = `sigs.k8s.io/controller-runtime`

**Decision.** Use [`controller-runtime`](https://github.com/kubernetes-sigs/controller-runtime) (the SIG-API-Machinery library underlying kubebuilder) for the controller-side scaffolding: `manager.Manager`, `controller.Controller`, `reconcile.Reconciler`, generic typed `client.Client`, plus the `Lease`-based `leaderelection` package.

**Why.** Standard in the K8s ecosystem; kubebuilder-compatible (we can adopt kubebuilder later if useful, or not); abstracts over the informer / workqueue / typed-client plumbing we'd otherwise hand-roll on top of raw client-go. Tested, well-trodden, and lets us focus on Ollie-specific reconcile logic instead of K8s plumbing.

**Rejected alternative.** Raw `client-go` with hand-rolled informers / workqueues. Workable for small controllers but reinvents what controller-runtime already does well; nobody builds new controllers this way in 2026.

### 22.3 Codegen = `controller-gen` only (no kubebuilder scaffolding, no client-gen)

**Decision.** Wire `controller-gen` into `ap generate //...` to produce:

- DeepCopy methods (`zz_generated_deepcopy.go`) for the CRD Go types.
- CRD YAML manifests from `+kubebuilder:` markers, output to `k8s/crds/`.
- RBAC YAML from `+kubebuilder:rbac:` markers on the reconciler types, output to `k8s/rbac/controller-generated.yaml`.

Do **not** use kubebuilder project scaffolding (would force a directory layout that fights our existing `pkg/controller/` shape). Do **not** use client-go's `client-gen` / `lister-gen` / `informer-gen` (controller-runtime's generic typed client covers all the access patterns the reconciler needs).

**Why.** Three reasons. (1) `controller-gen` is the de-facto K8s codegen tool — used by kubebuilder, Operator SDK, every modern controller framework. (2) Generation is markers-driven so the source of truth lives next to the types it describes, not in a separate YAML manifest. (3) `ap generate //...` already runs gofmt + a few other generators; adding `controller-gen` is one more entry, and `ap-verify-generate` will catch drift exactly the way it already does for gofmt.

**Consequences.** A `tools/tools.go` blank-import keeps `controller-gen` in `go.mod`; the `ap generate` integration invokes `controller-gen` over `pkg/controller/api/v1alpha1/...`. Generated files (`zz_generated_*.go`, `k8s/crds/*.yaml`, `k8s/rbac/controller-generated.yaml`) are committed and verified by `ap-verify-generate` in CI.

### 22.4 Validating admission webhook deferred from v0.4 to v0.5

**Decision.** v0.4 ships **no `ValidatingWebhookConfiguration`**. Issue [#90](https://github.com/gke-labs/in-cluster-observability/issues/90) is reassigned to the v0.5 milestone. Cross-resource semantic validation (overlapping selectors, sink-reference resolution, conflict detection) moves into the reconciler and surfaces as a `Conflict` Condition on CR status.

**Why.**

- **The CRD OpenAPI schema gives most of the webhook's value for free.** `controller-gen` turns `+kubebuilder:validation:*` markers into OpenAPI schema; the K8s API server validates against it on every apply. Required fields, type checks, enum constraints, regex patterns, min/max ranges, CEL `XValidation` rules — all checked at apply time without a webhook.
- **The remaining cross-resource checks work just as well in the reconciler.** "TrafficMonitor A and B select overlapping pods" can't be expressed in OpenAPI schema (multi-CR state), but the reconciler sees both CRs and emits a `Conflict` Condition on each within one reconcile tick. The operator's view is "apply succeeds, then `kubectl get trafficmonitor -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'` reads `False` with `Reason=ConflictsWith`" — same actionable error, no admission rejection.
- **Removes a runtime dependency from v0.4 install.** A webhook needs HTTPS, which needs a cert, which needs cert-manager or an equivalent bootstrap (~1 CRD + 2 controllers + an admission webhook of its own). Deferring means v0.4's `kubectl apply -k k8s/` brings up CRDs + RBAC + controller Deployment + Service. No cert-manager required.

**Consequences.** `docs/design/control-plane.md` §5 marked deferred with a pointer to this ADR. The reconciler design grows a `Conflict` Condition path (§9 of the same doc). v0.5 revisits when there is a concrete class of error the reconciler-based path cannot handle gracefully — until then, no webhook.

**Rejected alternative.** Self-signed cert bootstrap (no cert-manager) — workable but reinvents cert-manager poorly; rotation logic becomes our problem.

### 22.5 Identity broadcasting cut from v0.4 scope

**Decision.** v0.4's controller does **not** run a canonical K8s informer for identity, does **not** maintain an `IdentityCache`, and does **not** stream `IdentitySnapshot` / `IdentityDelta` messages over the agent gRPC stream. The proto reserves the message tags for forward compatibility but no payload is sent. Issues [#101](https://github.com/gke-labs/in-cluster-observability/issues/101), [#102](https://github.com/gke-labs/in-cluster-observability/issues/102), [#103](https://github.com/gke-labs/in-cluster-observability/issues/103) remain on the v0.5 milestone where they already lived.

**Why.** Per [ADR-0021](#adr-0021-lean-v03--agent-re-uses-obis-native-enrichment), OBI's K8s metadata informer attaches `k8s.namespace.name`, `k8s.pod.name`, `k8s.deployment.name`, `k8s.statefulset.name`, `k8s.replicaset.name`, `k8s.daemonset.name`, `k8s.node.name`, `k8s.container.name`, `k8s.pod.uid`, `k8s.pod.start_time`, `k8s.cluster.name` to every captured event natively — without the controller having to broadcast anything. The ADR-0009 hybrid-custody model was designed for the pre-ADR-0021 architecture where the agent did its own enrichment; under the lean v0.3 / v0.4 architecture there is no agent-side enrichment to feed identity into.

What remains is a smaller set of identity-needing use cases that *don't* coincide with OBI's informer:

- Off-cluster peer attribution on L4 flows (external DBs, managed services).
- `peer.k8s.*` enrichment on L7 spans (OBI's L7 uprobe path emits source-side identity only; L4 socket-filter mode gives dual-sided for free, so L7 is the gap).

Neither is a v0.4 blocker; both are v0.5 candidates once the in-cluster store gives the agent a richer write-time hook to consume identity at.

**Consequences.** `docs/design/control-plane.md` §3 retains a short note framing the deferral. The gRPC stream is simpler: `AgentSession (stream AgentMessage) returns (stream ControllerMessage)` carries only `MonitoringSpecDelta` + heartbeats. The controller's RBAC is correspondingly narrower — no `services`, no `endpointslices`, no informer for them (those would have fed the identity cache).

---

**Implemented in.** v0.4 milestone work, landing across four phases:

- Phase 0: this ADR + the `control-plane.md` refresh.
- Phase 1: CRD Go types + gRPC service stubs (closes #85, #86).
- Phase 2: reconciler + gRPC stream + leader election (closes #87, #88, #89).
- Phase 3: RBAC + CR status + AgentStatus feedback (closes #91, #92, #93).

Each phase ships as its own PR against `main` from a stacked branch on the user's fork (`mastersingh24/in-cluster-observability`).

---

## ADR-0023: Verification-first — v0.4.5 milestone gates v0.5

**Status:** Accepted, 2026-07-28

**Context.** A full project assessment (2026-07-28) found the design docs well ahead of the verification story, with three structural gaps between what the docs promise and what CI can actually check:

1. **No automated coverage of the OBI boundary.** [`testing-and-benchmarks.md`](testing-and-benchmarks.md) specifies a Kind-based e2e presubmit (`ap-e2e`); none exists for the ollie DaemonSet. The contract fixtures are the ADR-0019.6 synthetic seeds — they freeze the translator against itself, not against OBI's wire format, so an OBI image bump that changes OTLP shape passes CI silently. The two most consequential historical bugs (the `--config` vs `OTEL_EBPF_CONFIG_PATH` incident in ADR-0021; L7 silently no-oping without caps) were both found only by live runs.
2. **The `:9090` metrics path is not sound as a Prometheus source.** The OTLP translator never inspects `AggregationTemporality` or `IsMonotonic` (correctness silently depends on OBI exporting delta), histograms are reduced to their sum (no `rate()`/percentiles downstream), and the forwarder's counter-vs-gauge decision is a metric-name-suffix guess with zero test coverage.
3. **Lifecycle bugs in `pkg/capture`** (Stop-after-failed-Start deadlock, send-on-closed-channel panic in the recover path, receiver leak on partial start) sit in the package that v0.4's controller and v0.5's store both build on.

Meanwhile the OBI pin (v0.9.0) is one minor behind upstream (v0.10.0, docs updated 2026-07-20), and OBI permits breaking changes per minor.

**Decision.** Insert a **v0.4.5 "Verification & Soundness"** milestone between v0.4 and v0.5, sequenced **after** the v0.4 security remediation ([#143](https://github.com/gke-labs/in-cluster-observability/issues/143)–[#145](https://github.com/gke-labs/in-cluster-observability/issues/145)) lands. Scope, tracked in issues [#150](https://github.com/gke-labs/in-cluster-observability/issues/150)–[#156](https://github.com/gke-labs/in-cluster-observability/issues/156), [#158](https://github.com/gke-labs/in-cluster-observability/issues/158):

- Minimal Kind e2e presubmit exercising the real DaemonSet (#150).
- Contract fixtures recorded from a real OBI via the REGENERATE.md recorder pipeline (#151).
- OBI image bump v0.9.0 → v0.10.0 under that coverage, per the ADR-0010/0018 single-bump-PR policy (#152).
- OTLP temporality + histogram correctness in translator and forwarder (#153).
- `pkg/capture` lifecycle fixes (#154).
- DaemonSet production trim: probes, tolerations, priorityClass, PSA labels, seccomp, updateStrategy (#155).
- Build/CI hygiene: pinned `ap`, digest-pinned builder, `go mod tidy` (#156).
- Tracker hygiene: open-PR sweep + stale milestone-issue closure, explicitly gated on #143–#145 merging first (#158).

Two sequencing rules ride with this ADR:

- **v0.5 proceeds as a vertical slice**: tsdb HEAD (#78) → PromQL fan-out + query server (#94, #95) → `custom.metrics.k8s.io` (#96) → a working HPA demo, before the remaining v0.5 breadth (CEL streaming, OTLP push, `iobsctl`, identity). The HPA demo is the differentiation claim versus "just deploy Beyla/OBI directly"; it should exist as early as possible.
- **The library-first posture gets its own ADR before v0.5's public store/query interfaces freeze** ([#157](https://github.com/gke-labs/in-cluster-observability/issues/157)): post-ADR-0018, the capture layer is a container topology, not an embeddable Go capability, and requirements §2.4 / `public-api.md` need to say precisely what *is* embeddable. Stability tags on unimplemented `pkg/*` packages downgrade to Experimental as part of that work.

**Consequences.**

- ✅ The riskiest integration in the system (the OBI boundary) becomes CI-checkable before more layers stack on it; OBI bumps become judgeable PRs instead of faith-based ones.
- ✅ `:9090` numbers become trustworthy for rates and percentiles — or are demonstrably not yet, with the gap visible in CI rather than in a user's dashboard.
- ✅ v0.5 starts on a cleaner runway with its thesis demo (HPA on captured metrics) front-loaded.
- ⚠️ v0.5 starts later in wall-clock terms. Accepted: the assessment's judgment is that unverified foundations are the bigger schedule risk.
- ⚠️ One more open milestone in the tracker. Mitigated by #158's milestone-closure sweep.

**Rejected alternatives.**

- *Fold this work into v0.4.* v0.4 is nearly done and already grew the security remediation; growing it further delays its merge gate without benefit.
- *Fold it into v0.5.* Mixes verification debt with feature work and invites the debt to slip; the point is that these items *gate* v0.5.
- *Skip straight to v0.5 and verify later.* The ADR-0021 postmortem is the counterexample: two milestones of work sat on an integration that had never actually been exercised.

**Implemented in.** Milestone `v0.4.5 Verification & Soundness` and issues #150–#158, filed 2026-07-28.

---

## ADR-0024: Extensibility via wire protocols, not a Go library (resolves #157)

**Status:** Accepted, 2026-07-29. Supersedes the library clause of requirements §2.4, [ADR-0004](#adr-0004-library--controller-posture-public-api-in-pkg)'s "importable Go library" posture, and [ADR-0011](#adr-0011-sink-interface-shape)'s in-process sink interfaces.

**Context.** Requirements §2.4 made "third parties import the library and register their own sinks" load-bearing, as the differentiator versus Pixie. That posture was designed before [ADR-0018](#adr-0018-obi-as-sibling-container-not-embedded-library), when the plan was an in-process Go event pipeline embedders could hook. Three milestones later, the premise no longer describes the system:

- **Capture is a pod topology, not a Go capability.** An importer of `pkg/capture` gets a config writer and loopback OTLP receiver that are meaningless without the OBI sibling container deployed exactly as our DaemonSet deploys it. "Embedding" would be re-packaging our deployment.
- **The controller** is a controller-runtime app reconciling our CRDs — deployed, not embedded.
- **Sinks never needed Go.** The data is OTLP-shaped end to end (the reason OBI was chosen, [ADR-0001](#adr-0001-ebpf-data-plane--opentelemetry-ebpf-instrumentation-obi)). A "custom sink" is an OTLP endpoint we push to, a CEL-filtered streaming subscription, or a Prometheus scrape — all v0.5 deliverables, all consumable with zero Go coupling, including by the AI-agent consumers of requirements §4 who were never going to link a Go module.
- **The evidence agrees.** `pkg/sink`'s registry has no data path, `pkg/obsapi` constructs a no-op manager regardless of Role, `pkg/query` is an empty interface — decorative since v0.1, with zero external demand (no issues, no discussions). Meanwhile every load-bearing piece built since (bridge, collector, recorder) needed no public surface.
- **The cost is real.** Designing v0.5's store/query interfaces for hypothetical embedders taxes the HPA vertical slice; `Stability: Stable` tags on unimplemented packages promise semver we cannot honor; v1.0 would owe docs/examples for an API nobody uses.

**Decision.**

1. **Extensibility is delivered over the wire.** The supported extension surface is: OTLP push to operator-configured endpoints, the gRPC streaming query/subscribe API (CEL-filtered), the Prometheus scrape endpoint, and (v0.5) remote-write. "Adding a sink requires no fork" is satisfied by pointing the system at your endpoint, not by implementing a Go interface.
2. **The product is the deployable.** Requirements §2.4's "ships as both a controller and a library" is amended to "ships as a deployable system with open egress." The Pixie differentiation is restated as **open egress**: Pixie's data lives behind PxL and its store; ours leaves in standard protocols.
3. **The Go surface shrinks to what is genuinely reusable**: `pkg/schema` (constants) and the OTLP translators (`pkg/capture.TranslateMetrics/TranslateTraces`), all `Stability: Experimental`. Everything else is fair game to move under `internal/` as v0.5 touches it; no package carries `Stable` until it has an implementation and a consumer.
4. **v0.5 designs store/query for the binary**, under `internal/`, shaped by the HPA slice — not for embedders. If a concrete embedder materializes later, a library is extracted from working code then (cheap) rather than speculated now (expensive).
5. **Deletions over stubs**: `pkg/sink` (registry + interfaces), `pkg/obsapi`, and the `pkg/store`/`pkg/query` interface stubs are removed as v0.5 work items rather than implemented.

**Consequences.**

- ✅ v0.5's interfaces serve the HPA demo, not a hypothetical audience; the vertical slice gets faster.
- ✅ The differentiation claim becomes honest and stronger — wire protocols reach strictly more consumers than a Go API.
- ✅ Semver/docs burden at v1.0 shrinks to two small Experimental packages.
- ✅ Roadmap item 9 (WASM plugin model) is retired — the extension point it served no longer exists. Roadmap item 6's future first-party sinks become built-in egress features behind config, or external consumers of the stream.
- ⚠️ If a serious Go embedder appears, extraction is a deliberate later project. Accepted: extracting from working internals is cheaper than maintaining speculative API, and no such embedder exists today.
- ⚠️ `docs/design/public-api.md` and `sinks-and-extensibility.md` require rewrites; banners added now, full rewrite rides with v0.5.

**Rejected alternatives.**

- *Keep library-first as written.* Maintains a promise the architecture stopped supporting at ADR-0018; taxes every v0.5 interface; zero demonstrated demand.
- *Narrow to "embed sink/store/query only."* The middle path considered in #157. Still forces public API design onto v0.5's critical path for the same hypothetical audience; wire protocols serve every named consumer better.
- *WASM/plugin sinks (roadmap 9).* Solves in-process extension for non-Go — but there is no in-process extension point worth keeping once egress is wire-based.

**Supersedes.** Requirements §2.4 library clause (amendment note added in place); ADR-0004's library posture (its `pkg/` vs `internal/` layout convention stands); ADR-0011 in full (the three sink interfaces are deleted, not implemented).

**Implemented in.** This PR (docs). Code deletions and `internal/` moves land as v0.5 work items.

---

## ADR-0025: v0.5 vertical-slice implementation decisions

**Status:** Accepted, 2026-07-29.

**Context.** v0.5 ships as a vertical slice — embedded metric store ([#78](https://github.com/gke-labs/in-cluster-observability/issues/78)) → query server + PromQL fan-out ([#94](https://github.com/gke-labs/in-cluster-observability/issues/94), [#95](https://github.com/gke-labs/in-cluster-observability/issues/95)) → `custom.metrics.k8s.io` ([#96](https://github.com/gke-labs/in-cluster-observability/issues/96)) → HPA demo — under [ADR-0024](#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157)'s posture (store/query designed for the binary, under `internal/`). The issue texts and [`storage-and-query.md`](storage-and-query.md) predate ADR-0021 and ADR-0024: they reference `pkg/store`/`pkg/query`/`pkg/sink/custommetrics` surfaces that no longer exist, an enricher pipeline that was never built, and cert-manager TLS that belongs to the v0.6 hardening milestone. This ADR pins the implementation shape so the slice can land without re-litigating each deviation per PR. Like ADR-0022, it records sub-decisions individually.

**Decision.**

1. **The metric store lives in `internal/store` and embeds the full `tsdb.DB` via `tsdb.Open`, not a raw `head.NewHead`.** Options: `MinBlockDuration = MaxBlockDuration = 2m`, `RetentionDuration = 10m`, WAL enabled — the ADR-0012 numbers stand. The DB manages WAL replay, head→block compaction, and retention deletion; hand-assembling those around a bare HEAD (storage-and-query.md §2.1's plan) re-implements tsdb lifecycle for no gain. Data dir `/var/lib/ollie/tsdb` on an `emptyDir` volume: with 10-minute retention, durability across pod deletion is worthless; the WAL still recovers from container restarts within the pod's life.
2. **Ingest is a registry self-scrape, not an event-dispatch write path.** A 1 s loop gathers the agent's in-process Prometheus registry (self-obs metrics + the OBI const-metric collector from #153) and appends every sample to the tsdb head. One normalization path: whatever `/metrics` on `:9090` serves is, by construction, exactly what PromQL sees. storage-and-query.md §2.4's `WriteMetric`-per-event design assumed the deleted enricher pipeline; the self-scrape replaces it. Histograms land as classic `_bucket`/`_sum`/`_count` series.
3. **Fan-out happens at the storage layer, not the query layer.** *(Amended in implementation, 2026-07-29: the per-agent series-select wire is the standard Prometheus **remote-read** protocol — `POST /api/v1/read` on `:9091`, served and consumed by stock `prometheus/storage/remote` code — rather than a bespoke `proto/query/v1` gRPC service. Identical fan-out semantics, zero custom wire format, and it doubles as an ADR-0024 egress surface: any Prometheus can remote-read an agent.)* The agent exposes a series-select endpoint backed by the local tsdb querier. The query server implements Prometheus `storage.Queryable` by fanning `Select` out to every agent and merging series, then runs the stock `promql.Engine` centrally over the merged view. This gives exact PromQL semantics for free and supersedes storage-and-query.md §5.1's per-node partial-aggregation scheme ("partials are themselves rates and summed"), which silently mis-aggregates non-decomposable queries (`histogram_quantile`, `topk`, subqueries). The §5.3 degraded contract stands: per-agent deadline 0.8× overall; misses annotate the response with `degraded=true` + `missing_nodes`.
4. **The query server is a separate binary, `cmd/ollie-query`.** Follows the `cmd/ollie-controller` precedent. #94's `--role=query` flag on `cmd/ollie` came from the `obsapi.Role` design deleted by ADR-0024; one binary per deployable keeps images minimal and flags disjoint.
5. **Agents are discovered via a headless Service.** `k8s/agent-service.yaml` (`clusterIP: None`, selector = agent pods) gives per-pod DNS A records; the query server re-resolves on an interval. No controller registry coupling, no `AgentHello` proto change; if per-agent metadata is ever needed the proto has reserved field numbers.
6. **The custom-metrics server is hand-rolled and minimal, in `internal/custommetrics` inside `ollie-query`.** It serves `v1beta1` discovery plus the namespaced-object metric GET paths the HPA uses, mapping metric names to PromQL templates from a ConfigMap (defaults per storage-and-query.md §7). `kubernetes-sigs/custom-metrics-apiserver` (and its full `k8s.io/apiserver` dependency tree) is rejected for now per the minimize-deps convention — the HPA read path is small. Revisit if [#111](https://github.com/gke-labs/in-cluster-observability/issues/111) (aggregation hints) outgrows it.
7. **v0.5 TLS posture: self-signed at startup, verification deferred to v0.6.** The aggregation layer requires HTTPS on the backend, so `ollie-query` generates a self-signed serving cert at startup and the `APIService` registers with `insecureSkipTLSVerify: true`; front-proxy auth headers are accepted without CA verification, with the default-deny NetworkPolicy bounding exposure. #94's cert-manager `Certificate` and #96's CA-bundle injection move to v0.6 (TLS milestone), where they belong with the rest of transport hardening.
8. **Slice scope: metrics only.** The span/edge ring buffer (#84) and WAL fsync/snapshot work (#79) are not on the HPA path and land in a later leg. `pkg/topology` is deleted with the other ADR-0024 stubs; an `internal/` identity type returns with its first consumer (#84, #101–#103).

**Consequences.**

- ✅ Every PromQL construct works against the fan-out from day one — no aggregation-shape allowlist, no wrong quantiles.
- ✅ `:9090` scrape output and query-server results cannot drift; contract-test goldens cover both.
- ✅ The HPA demo needs zero new external infra (no cert-manager in the Kind e2e).
- ⚠️ Central PromQL evaluation ships raw series over the network; at 500 series/node × tens of nodes this is well within budget, and matcher pushdown already prunes. Revisit with pushdown aggregation only if benchmarks demand (roadmap item).
- ⚠️ Registry self-scrape quantizes ingest to 1 s and re-appends unchanged series each tick; tsdb dedups identical consecutive samples cheaply. Acceptable at target cardinality (§2.3 sizing).
- ⚠️ An unverified aggregated API backend is a known, bounded v0.5 gap, closed by the v0.6 TLS work.

**Rejected alternatives.**

- *Raw `head.NewHead` per storage-and-query.md §2.1.* Re-implements compaction/retention/WAL-replay orchestration tsdb already ships; the "no historical blocks" goal is a retention setting, not an architecture.
- *Per-node partial aggregation per §5.1.* Correct only for a decomposable subset of PromQL; the failure mode (silently wrong numbers feeding an autoscaler) is the worst kind.
- *Agent discovery via the controller's `AgentSession` registry.* Couples the read path to control-plane liveness and leader election; DNS needs neither.
- *`kubernetes-sigs/custom-metrics-apiserver`.* Correct long-term candidate, but drags the full apiserver library set into a repo that has stayed lean, for one read-only demo path.

**Implemented in.** v0.5 phase PRs (`v0.5/phase-0-design` through `v0.5/phase-3-hpa`).

---

## ADR-0026: v0.5 breadth implementation decisions

**Status:** Accepted, 2026-07-29.

**Context.** The remainder of the v0.5 milestone — egress push (#97, #98), streaming subscribe (#99), `iobsctl` (#100), the span/edge ring (#84), WAL/compaction tuning (#79), the identity trio (#101–#103), and the validating webhook (#90) — was specified before ADR-0021 (OBI native enrichment), ADR-0024 (wire-protocol extensibility), and ADR-0025 (vertical-slice shape). Like ADR-0025, this ADR pins the deltas once. Two pieces of recorded evidence drive the biggest cuts: the contract fixtures from the pinned OBI image show **dual-sided K8s attribution is native on L4 flows** (`k8s.src.namespace/owner.name/owner.type` + `k8s.dst.*` on `obi.network.flow.bytes`), and spans carry `client.address`/`server.address`.

**Decision.**

1. **#101 (identity broadcast) and #102 (fallback informer) are closed as superseded, not implemented.** Both exist to feed an agent-side IP→identity resolver (`IdentityCache`, `topology.PeerResolver`) that ADR-0021 removed — OBI's informer resolves identity in-process and stamps both sides of L4 flows. Building a controller→agent identity plane to enrich records the capture layer already enriches would be pure architecture re-creation. ADR-0009's fallback-informer design rides along: with no agent-side resolver, there is nothing for a fallback informer to feed. Reopen condition (per the ADR-0009 note): a consumer needs peer attribution OBI doesn't provide — L7 *metric* peer identity, or external-IP classification beyond what `k8s.dst.*` absence implies.
2. **#103 (peer enrichment + cardinality guard) closes with the same evidence.** OBI attributes peers at **owner granularity** (`k8s.dst.owner.name`, not pod name or IP), so the metric-surface cardinality the guard was designed to cap is bounded by construction; raw peer IPs never reach the label allowlist (#144 excludes them deliberately). The unpursued remainder — labeling off-cluster peers `peer.external=true` on metrics — moves to the roadmap with the reopen condition above.
3. **#90 (validating webhook) moves to v0.6.** A `ValidatingWebhookConfiguration` requires a CA bundle — there is no `insecureSkipTLSVerify` escape hatch for webhooks — so it lands with the v0.6 TLS milestone's CA machinery instead of growing a bespoke cert-injector now. `failurePolicy: Fail` with a self-managed cert is also an availability hazard we should not ship half-hardened.
4. **#79 is largely satisfied by the ADR-0025 §1 choice of `tsdb.Open`**: the DB already runs head→block compaction on schedule and enforces retention on each cycle. What lands: the two §8 self-obs metrics (`ollie_store_compactions_total`, `ollie_store_wal_fsync_seconds`) re-exported from tsdb's internal registry through an allowlist bridge (keeping the other ~100 tsdb series off the bounded `:9090` surface), and a block-rotation/retention test. **Deviation:** WAL fsync stays on tsdb's segment/page policy rather than per-batch — tsdb exposes no per-commit fsync knob, and the crash-loss window (one in-memory WAL page) is within the §2.5 30-second budget.
5. **#84 becomes a spans-only, in-memory ring of raw OTLP.** A new **raw tee** on the capture bridge hands the untranslated `Export*ServiceRequest` payloads to consumers; the ring stores `(resource attributes, tracepb.Span)` so CEL filters compile against the OTLP field paths users already know (storage-and-query.md §4.2) with zero translation loss. No span WAL: the consumers are live subscribers (#99) and ad-hoc queries (#100); replaying a 64k-entry, 10-minute window after a crash protects nothing a consumer relies on, and the WAL machinery is the most expensive part of the original spec. **Edges are deferred until a producer exists** — `EdgeEvent` has been an empty stub since v0.2, OBI emits no edge records, and the L4 flow *metrics* (dual-attributed, queryable via PromQL) already answer "who talks to whom."
6. **#97/#98 are relays, not re-encoders.** `internal/export` pushes the **original OTLP payloads** from the raw tee to operator-configured endpoints — the agent received OTLP, so re-encoding translated events back into OTLP would only lose fidelity. Per-endpoint bounded queue (1024 batches, drop-on-full with `ollie_export_*` accounting), exponential backoff, gRPC and HTTP protocols. **Configuration is agent flags** for v0.5 (`--export-otlp-*`); the CRD surface (sinks-and-extensibility.md open question 3) waits for the control-plane integration to have a real consumer. **Prometheus remote-write v1 rides along** as a third exporter kind, encoding the same gathered-sample stream the store ingester already produces — one normalization path, per ADR-0025 §2.
7. **#99 ships spans + metrics streaming; CEL evaluates on the agent.** `proto/stream/v1` carries serialized OTLP spans (bytes) plus resource attributes and a gap counter. The query server multiplexes per-agent streams (same discovery as the read fan-out); the CEL program is compiled and applied **agent-side** so non-matching spans never cross the network. Slow consumers get drop-oldest with gap markers, never backpressure into capture. `StreamEdges` is omitted with #84's edge deferral; `StreamMetrics(promql, step)` is periodic instant evaluation on the query server.
8. **#100 (`iobsctl`) speaks the public surfaces only** — the PromQL HTTP API and the streaming gRPC service via auto port-forward — proving the ADR-0024 claim that first-party tooling needs no private path.

**Consequences.**

- ✅ The leg's code lands where consumers are (ring→stream→CLI as one pipeline), not as speculative infrastructure.
- ✅ Three issues close on recorded fixture evidence instead of re-implementing what the ADR-0018/0021 architecture already provides.
- ⚠️ No span durability across agent restarts. Accepted: subscribers observe a gap either way; a WAL would only replay history nobody can have missed less recently than the ring window.
- ⚠️ External peers remain unlabeled on metrics (`k8s.dst.*` absent ⇒ off-cluster, by inference). Accepted until a consumer needs it explicit; reopen per §2.
- ⚠️ Webhook validation gap persists through v0.5 — invalid CRs are caught by CRD schema only. Accepted; #90 rides v0.6.

**Rejected alternatives.**

- *Implement #101–#103 as written.* Rebuilds the pre-ADR-0021 enricher architecture alongside the one that replaced it; the fixtures show the output would be redundant at the granularity any current consumer uses.
- *Span WAL per #84.* Cost concentrated exactly where the design's own consumers (live streams) cannot benefit.
- *Translate-then-re-encode export.* Strictly worse than relaying the received bytes.
- *Central CEL evaluation on the query server.* Ships every span off every node to filter most of them out; agent-side filtering is the whole point of having a per-node buffer.

**Implemented in.** v0.5 phase PRs `v0.5/phase-4-breadth-design` through `v0.5/phase-8-iobsctl`.

---

## ADR-0027: v0.5.1 hardening — adversarial review closes real defects the green-CI self-merge masked

**Status:** Accepted, 2026-07-29.

**Context.** With v0.5 merged and the milestone closed, the merged tree was put through an adversarial multi-agent review (14 agents across 7 independent lenses, Opus 5). It surfaced 24 findings that survived a verify stage, ~18 distinct after dedup. Green CI plus self-merge plus "milestone closed" had masked real problems — two of which undercut ADR reasoning recorded days earlier:

- A **`:6443` custom-metrics auth bypass**, found independently by three lenses. `cmd/ollie-query` mounted the aggregated API bare — no client-cert verification on the TLS listener — and `k8s/networkpolicy.yaml` opened `:6443` ingress with no `from:` selector. Any pod with zero RBAC could read cross-namespace traffic metrics and drive an unauthenticated PromQL fan-out, bypassing the kube-apiserver RBAC that gates the custom-metrics API. ADR-0025 §7 had justified deferring cert verification "because the default-deny NetworkPolicy bounds exposure" — **that justification was false as written**: the policy left this port open cluster-wide.
- The **L4 flow metric collapsed its `direction` attribute** (`direction` was not in the #144 allowlist), overwriting request/response/unknown datapoints last-write-wins on a *monotonic* counter → silent loss + counter step-downs → spurious `rate()` spikes. This is the exact metric ADR-0026 §5 cited ("L4 flow metrics … already answer who talks to whom") to justify closing #101–#103. The native labels are intact, but the "queryable via PromQL" claim was compromised until fixed; the #101–#103 closure rationale was partly built on a corrupted metric.

**Decision.** Open a **v0.5.1 hardening phase** that fixes the one CRITICAL and the six HIGH correctness defects immediately, with unit tests, under the same green-CI self-merge flow; defer the MEDIUM/LOW backlog to the v0.6 Hardening leg. Fixed now:

1. **`:6443` client auth (CRITICAL).** The listener requires an aggregation-layer front-proxy (requestheader) client cert: `--custom-metrics-client-auth` loads the requestheader CA from `kube-system/extension-apiserver-authentication` (new `internal/frontproxy`), pins `ClientCAs` + `RequireAndVerifyClientCert`, and gates the handler on the allowed CN. Only the kube-apiserver holds a cert signed by that CA, so an open port is not an open door. Per-user delegated SAR stays deferred (kube-apiserver enforces HPA RBAC before proxying); the serving-cert CA bundle stays with v0.6 per ADR-0025 §7.
2. **Fan-out serial execution + compounding deadline + primary-querier failure abort.** Agents register as **secondary** queriers (`NewMergeQuerier(nil, queriers, …)`) — concurrent Select, per-agent errors become warnings. A mid-stream `STREAMED_XOR_CHUNKS` failure (surfaced via `SeriesSet.Err()`, which the secondary path does *not* downgrade) is swallowed by an `agentSeriesSet` wrapper that records the agent as a miss and continues degraded rather than aborting the eval.
3. **Cross-node series merge.** The ingester stamps each self-obs series with a `k8s_node_name` node-identity label so identical series from N nodes no longer chain-merge into one interleaved series (workload metrics were already safe via per-pod `k8s.*`).
4. **NaN/Inf → HPA garbage.** The custom-metrics adapter treats a non-finite PromQL result as "no value" (404) instead of coercing it to a garbage int64 via `NewMilliQuantity`, so the HPA holds its replica count.
5. **Forwarder label-schema pinning.** The first-seen label set is no longer frozen for the process lifetime; a later datapoint carrying additional keys widens the schema to the union and migrates existing series onto it, so no key is silently dropped node-order-dependently.
6. **L4 `direction` collapse.** `direction` is added to the #144 `ForwardAllowedLabelKeys` allowlist so the three directions stay distinct series; this restores the ADR-0026 §5 closure rationale for #101–#103.

**Deferred to v0.6** (recorded so they are not lost with the review context):

- *MEDIUM* — HPA `metricLabelSelector` silently dropped (unfiltered aggregate returned under the requested name); CEL runtime error tears down a whole span stream (the documented example filter throws on spans lacking the key); span ring is drop-*newest* while the proto/ADR-0026 §7/its own comments promise drop-*oldest*; `TestIobsctl` is false-green (asserts only a header that prints on empty results); agent memory limit still 200Mi vs the sizing doc's 400Mi after the tsdb head + span ring landed (OOM risk).
- *LOW* — query-server `SubscribeSpans` hangs open silently when all upstreams end (masks auth failure as "no traffic"); `StreamMetrics` returns non-retriable `InvalidArgument` on transient errors; no staleness markers on ingest/remote-write; readiness probe tests only `:9090` so a WAL-replaying pod joins the fan-out before `:9091` is up; namespace pseudo-resource advertised in discovery but unrouted; `services→service_name` doesn't map to the K8s Service name.

**Consequences.**

- ✅ The security hole and the six correctness bugs are closed with regression tests before v0.6 builds on the surface.
- ✅ ADR-0025 §7 and ADR-0026 §5 are corrected in place — the NetworkPolicy is no longer load-bearing for `:6443` authn, and the L4 metric is sound again.
- ⚠️ The MEDIUM/LOW backlog rides v0.6; the drop-newest span ring and the memory-limit gap are the most user-visible of these.
- 📌 **Process signal:** green CI + self-merge did not catch a cluster-wide auth bypass or a corrupted-metric regression. The adversarial review is worth repeating at milestone close, not only mid-flight.

**Implemented in.** `v0.5.1/phase-1-hardening`.

---

## ADR-0028: v0.6 self-managed CA for the custom-metrics serving cert (#196)

**Status:** Accepted, 2026-08-01.

**Context.** v0.5 shipped the custom.metrics.k8s.io backend on `:6443` with a *serving*-side shortcut (ADR-0025 §7): `ollie-query` minted an ephemeral in-memory self-signed cert at startup and the APIService registered with `insecureSkipTLSVerify: true`. Client auth was **not** shortcut — v0.5.1/ADR-0027 §1 closed the `:6443` bypass with front-proxy mTLS. What remained was the serving side: with `insecureSkipTLSVerify` the aggregator does not verify the backend cert at all, so a MITM on the in-cluster hop to `ollie-query` is undetectable, and the deferral was explicitly parked for v0.6. Gari's constraint for this leg: **no cert-manager dependency** — a self-managed CA.

The single hard hazard is the transition. Dropping `insecureSkipTLSVerify` while any query replica still serves a cert the aggregator can't verify makes the `custom.metrics.k8s.io` APIService go `Unavailable`, which breaks every HPA reading through it and can disturb API discovery. So the flip from `insecureSkipTLSVerify:true` to `false` must be **atomic with** a valid `caBundle` and a serving cert that actually chains to it, on **every** endpoint.

**Decision.** A self-managed CA owned by `ollie-controller`, with the flip gated on observed endpoint state.

1. **CA lives in the controller, under leader election.** A new `internal/ca` package holds the pure crypto (mint CA, issue CA-signed serving certs, verify a served leaf chains to the CA) — cluster-free and unit-tested, since the integration path can only run in CI. A `ca.Manager` runs as a controller-runtime `Runnable` with `NeedLeaderElection() == true`, so exactly one replica writes cluster state; no two-writer race. This one mechanism also serves #90's webhook caBundle later — the decisive reason to put CA ownership here rather than have `ollie-query` self-bootstrap (which would need its own Lease and a secrets-write + apiservices-patch grant on the query SA).
2. **Storage.** `ollie-ca` Secret (cert **and** key; the key never leaves the controller) and `ollie-query-serving` Secret (leaf + key, plus `ca.crt` for #197 consumers), both `kubernetes.io/tls`, both in the install namespace. The CA is long-lived (5y); serving certs are short (90d) and renewed at ~⅓ life on the resync loop.
3. **Serving cert consumption.** `ollie-query` loads the serving cert from the mounted Secret via `tls.Config.GetCertificate` with an mtime-triggered hot reload, so a rotation is picked up without a restart. The volume is `optional: true` and `GetCertificate` **falls back to the old self-signed bootstrap cert** when the Secret is not yet mounted — a fresh `kubectl apply -k` never blocks query startup on the controller, and `ClientCAs`/`RequireAndVerifyClientCert` (ADR-0027 §1) are left untouched: the serving cert (`Certificates`→`GetCertificate`) and the front-proxy client-auth are orthogonal fields trusting different CAs.
4. **caBundle and the flip are one atomic, gated commit.** The aggregation API server enforces an invariant we cannot design around: `spec.insecureSkipTLSVerify: may not be true if caBundle is present`. So the two fields *cannot* be split across reconcile passes — there is no "write the caBundle now, flip later" intermediate state; the API rejects it. The manager therefore holds the shipped bootstrap posture (`insecureSkipTLSVerify: true`, **empty** `caBundle` — which never breaks an HPA) untouched until it has probed every *ready* `ollie-query` endpoint on `:6443` and confirmed each presents a leaf that chains to the current CA, then in a **single MergePatch** sets `caBundle` **and** clears `insecureSkipTLSVerify` at once. Greenfield satisfies the gate on the first pass with endpoints up; an upgrade from the v0.5 self-signed posture waits out the query rollout. The manifest ships `insecureSkipTLSVerify: true` as the safe bootstrap default — the controller owns the transition, and it self-heals (the same gated commit also re-asserts the caBundle should the CA ever change). The APIService is patched through the **dynamic client** (`apiregistration.k8s.io/v1` is not otherwise a dependency; adding kube-aggregator's typed clientset was not worth it).
5. **The flip-gate probe forces TLS 1.3.** `:6443` requires a client cert, but under TLS 1.3 the server's client-cert check is *post-handshake*, so a certless probe still completes the handshake and observes the served cert (and never sends a request). Forcing `MinVersion: TLS 1.3` on the probe avoids the TLS 1.2 mid-handshake abort — the same post-handshake semantics documented in the Phase-1b `TestAuthBoundaries` probe.

**Scope.** This ADR covers #196 (serving cert + caBundle + flip). #197 (intra-ollie TLS on the plaintext `:9091`/`:9092`/`:9095`/`:9096` hops) and #90 (validating webhook) ride the same CA machinery in follow-up PRs. #198 (per-user delegated SAR) stays deferred: kube-apiserver enforces the HPA controller's RBAC before proxying, and `:6443` already authenticates the aggregator by mTLS, so it adds little until non-HPA callers hit `:6443` directly.

**Consequences.**

- ✅ The aggregator verifies the `ollie-query` serving cert against a CA ollie controls; `insecureSkipTLSVerify` is gone in steady state, closing the ADR-0025 §7 serving-side deferral without cert-manager.
- ✅ The takedown hazard is structurally prevented: the atomic caBundle+flip commit cannot happen until observed endpoint state proves it is safe, so a bad SAN or an in-progress rollout degrades to "still insecureSkipTLSVerify, empty caBundle" rather than "APIService Unavailable". The API server's own `caBundle`-xor-`insecureSkipTLSVerify` invariant makes any half-applied state impossible.
- ✅ Serving-cert rotation is restart-free; the CA + `internal/ca` crypto are reused by #197/#90.
- ⚠️ **CA rotation** is not yet automated: the CA is minted once and a corrupt `ollie-ca` Secret is surfaced, not silently overwritten (overwriting would strand the distributed `caBundle`). Automatic CA rotation needs a two-phase dual-CA `caBundle` overlap and is left for a follow-up.
- 📌 New RBAC: controller gains cluster `apiservices` get/list/watch/update/patch and a namespaced Role (secrets CRUD + endpoints get) in the install namespace — its only Secret access is the two objects it owns. e2e proof is `TestCustomMetricsTLSVerification`: caBundle injected, `insecureSkipTLSVerify` dropped, and the APIService `Available` **under real verification** — which `insecureSkipTLSVerify` masked in v0.5.

**Implemented in.** `v0.6/phase-2-tls-ca`.

---

## ADR-0029: v0.6 intra-ollie TLS — every internal hop encrypted, clients fail closed (#197)

**Status.** Accepted (2026-08-02).

**Context.** v0.5 left the intra-ollie hops as bearer-token over plaintext: agent `:9091` remote-read and `:9092` span stream (dialed by `ollie-query`), and query `:9095` PromQL API and `:9096` stream mux (dialed by `iobsctl` and other subscribers). NetworkPolicy bounds who can connect and tokens gate what they can do, but the tokens themselves — cluster-scoped read credentials — transit unencrypted on the pod network. ADR-0028 built the machinery that closes this: a self-managed CA in the controller, CA-issued serving certs in Secrets (already carrying `ca.crt` for exactly this consumer), and a hot-reloading `GetCertificate`.

**Decision.** All four hops serve TLS unconditionally, and the in-cluster clients verify it, failing closed.

1. **One shared serving cert per component, DNS SANs over IP SANs.** The controller's CA manager issues a second serving Secret, `ollie-agent-serving`, alongside `ollie-query-serving` — same issuance/renewal path, same `ca.crt` distribution, one new `ensureServingCert` call. Every agent pod shares the one keypair. Fan-out clients dial agents by *pod IP* (headless-Service A records), so per-pod IP SANs would need per-pod certs and reissuance on every reschedule; instead the cert carries the headless-Service DNS forms (`ollie-agent.<ns>.svc[.cluster.local]`) and clients set `tls.Config.ServerName` to the same FQDN they resolved. Verification proves "holds a key the ollie CA signed for the agent Service", which is the actual trust statement we need; per-pod identity adds nothing an attacker on the pod network couldn't also satisfy by compromising the shared DaemonSet Secret.
2. **Servers never speak plaintext.** Agent `:9091`/`:9092` and query `:9095`/`:9096` adopt the ADR-0028 §3 posture via a shared `ca.ServingTLSConfig` helper: the mounted CA-issued keypair when present (hot-reloaded on rotation), a self-signed bootstrap cert until then (`optional: true` Secret mount; fresh installs and `go run` never block on the controller). `:6443` now shares the same `tls.Config` base with its front-proxy `ClientCAs` layered on top.
3. **Clients verify or degrade — no skip-verify middle ground.** `ollie-query` pins `ca.crt` from its own mounted serving Secret (`--agent-ca-file`) with `ServerName` = the agent Service FQDN, for both the remote-read fan-out (Prometheus `HTTPClientConfig.TLSConfig`) and the stream mux (per-dial gRPC creds). While the CA file is missing or an agent still serves its bootstrap cert (first-install window, one kubelet Secret-sync at most; upgrades have no window because the Secrets pre-exist), dials fail and surface through the *existing* degraded machinery (`degraded:true` + missing nodes; stream subscribers get `Unavailable` and resubscribe) — never a silent plaintext or unverified fallback. The only opt-outs are explicit: `--agent-ca-file=""` (or the unmodified default path absent *outside* a cluster) selects plaintext for dev.
4. **`iobsctl` verifies against the CA from the Secret.** The CLI already holds a kubeconfig; it reads `ca.crt` from `<service>-serving` and pins `ServerName` to `<service>.<ns>.svc` (the port-forward dials 127.0.0.1, so SNI must be overridden). Users without Secret read permission get an actionable error and an explicit `--insecure-tls` escape hatch — encrypted either way, and the tunnel itself already rides the user's authenticated API-server connection.
5. **`:9090` scrape deliberately stays plaintext.** It is the one hop consumed by *foreign* clients (GMP and other Prometheus scrapers), where mandating our CA would break drop-in scrape configs; token auth (#145) + NetworkPolicy (#143) already gate it, and the payload is metrics, not credentials with cluster-wide read scope. Revisit only if a scraper-side CA-distribution story appears.

**Consequences.**

- ✅ Bearer tokens no longer transit the pod network in the clear on any ollie-to-ollie hop; a pod-network sniffer gets TLS on `:9091`/`:9092`/`:9095`/`:9096`.
- ✅ Zero new trust anchors or dependencies: same CA, same Secrets, same reload machinery as ADR-0028; the controller change is one more `ensureServingCert` call plus SANs.
- ✅ Fail-closed by construction: a missing CA file or an unverifiable peer produces the already-tested degraded/unavailable behavior, not plaintext. e2e proof: `TestIntraTLSVerification` (strict RootCAs+ServerName round-trips against `:9095` and `:9091`) and `TestIobsctl` (CA-verified `:9095`/`:9096` through the real CLI path).
- ⚠️ First-install bootstrap window: until the controller issues Secrets and kubelet mounts them (≈ one Secret-sync interval), queries report degraded and streams are unavailable. Judged acceptable — it is self-healing, upgrade installs skip it entirely, and the alternative (temporary skip-verify) is a downgrade path an attacker could hold open.
- ⚠️ All agents share one serving keypair (DaemonSet-mounted Secret). Compromise of one node's Secret impersonates any agent — but that Secret is namespace-scoped and node-local compromise already yields the node's kubelet creds; per-pod certs would not change the outcome.
- 📌 `iobsctl` now needs Secret `get` in the install namespace for full verification (or `--insecure-tls`). Kubelet probes on `:9095` switch to `scheme: HTTPS` (kubelet skips verification, so the bootstrap cert is fine).

**Implemented in.** `v0.6/phase-2b-intra-tls`.

---

## ADR-0030: v0.6 validating admission webhook on the self-managed CA, enforced only after verification (#90)

**Status.** Accepted (2026-08-02).

**Context.** #90 (deferred from v0.4 per ADR-0022, then from v0.5) wants admission-time rejection of semantically invalid `TrafficMonitor`/`ClusterTrafficPolicy` objects. The original blocker — "a webhook needs HTTPS, which needs cert-manager" — dissolved when ADR-0028 shipped the self-managed CA. Two constraints shape the design: (a) the issue predates the CRD schema and the reconciler's reactive Conflict conditions (ADR-0022.4), which already cover most of its list — only checks the schema *cannot* express belong in the webhook; (b) several of the issue's validations (sampling-rate ranges, path-templating RE2, sink catalogs) reference fields that only land with #108/#109, so v0.6 ships the webhook *infrastructure* plus today's checks, and later fields slot into the same validator.

**Decision.**

1. **Webhook serves from the controller, on every replica.** controller-runtime's webhook server on `:9443` in `ollie-controller` — the binary that already owns the CRDs, the cache (pods + CRs for the soft checks), and the CA. Serving posture is ADR-0029 §2 verbatim: `ca.ServingTLSConfig` with a new CA-issued `ollie-webhook-serving` Secret (third `ensureServingCert` call), optional mount, self-signed bootstrap fallback. Pod readiness gates on the webhook listener so a replica never receives admission traffic before it can answer.
2. **Enforcement is a gated atomic commit, mirroring the APIService flip.** The manifest ships the `ValidatingWebhookConfiguration` with **empty `caBundle` and `failurePolicy: Ignore`** — the safe bootstrap posture: a webhook that is not up yet, mid-rollout, or freshly reset by `kubectl apply -k` can never block CR writes. The CA manager (leader) probes every ready webhook endpoint and, once each serves a CA-signed leaf, patches **`caBundle` + `failurePolicy: Fail` for all webhook entries in one write**, re-asserted every resync. There is no configuration where `Fail` points at an unverifiable backend. During the Ignore window, invalid CRs that slip through are caught reactively by the reconciler's Conflict conditions — the pre-webhook status quo, now bounded to bootstrap.
3. **Validation split.** The CRD OpenAPI schema keeps type/range/enum checks. The webhook **rejects** what the schema cannot: incoherent label selectors (`LabelSelectorAsSelector` failures — bad operators, `Exists` with values) and duplicate or out-of-range HTTP ports. It **warns, never rejects,** on legal-but-suspicious states that may be transient during rollouts (control-plane.md §3): no protocol enabled, ports set with `http.enabled: false`, selector matching zero pods, selection overlapping a sibling TM. Warnings ride the admission response into kubectl output. Soft checks read the manager cache and are skipped (not failed) on cache errors — a cache hiccup must not block writes. Deletes are never blocked.

**Consequences.**

- ✅ #90's acceptance shape holds in steady state: invalid CRs are rejected at apply time with field-level errors; zero-match selectors are accepted with a warning. e2e proof: `TestWebhookValidation` (waits for the gated `Fail` commit, then asserts one rejection per class and the warning-only path).
- ✅ No new trust anchors, no cert-manager, no bricking: the failure mode of every bootstrap/rollout state is "webhook silently absent" (Ignore), never "CR writes blocked". `kubectl apply -k` resets heal within one resync, same as the APIService.
- ⚠️ `failurePolicy: Fail` + our own CRs means the controller can block its operators' CR writes if all replicas are down *after* enforcement. Judged acceptable: 2 replicas, readiness-gated endpoints, and the failure is scoped to ollie's own CRDs (never core resources). Deleting the `ValidatingWebhookConfiguration` is the documented break-glass.
- ⚠️ The Ignore→Fail transition means admission is *eventually* enforcing, not install-time enforcing. Anything admitted during bootstrap is validated only reactively (Conflict conditions). This is the deliberate trade against blocking CR writes with an unreachable webhook.
- 📌 RBAC: controller gains `validatingwebhookconfigurations` get/update/patch, `resourceNames`-scoped to `ollie-webhook`. New cluster object: `ollie-webhook` (kustomize-managed); `ollie-controller` Service gains port 9443. #108/#109 field validations extend `validateCore` when those fields land.

**Implemented in.** `v0.6/phase-2c-webhook`.

---

## ADR-0031: v0.6 Phase 3 protocol support — gRPC, HTTP/2, TLS (#104–#107)

**Status.** Accepted (2026-08-03).

**Context.** Phase 3 of the v0.6 leg turns on the L7 protocols beyond HTTP/1.1: HTTP/2 (#104), gRPC (#105), and TLS decryption for Go `crypto/tls` (#106) and OpenSSL (#107). The issues were filed against assumptions about what the pinned OBI image emits — #104 wanted an `http_version=2`-labelled golden; #105 assumed semconv attributes `rpc.system`, `rpc.service`, and `rpc.grpc.status_code`. A source audit of OBI at the pinned `v0.10.0` tag (it pins `go.opentelemetry.io/otel/semconv/v1.41.0`) contradicted several of those assumptions, so the phase was re-scoped to match ground truth rather than the filed acceptance text.

**Ground truth (OBI v0.10.0).**

- **No HTTP protocol-version label.** OBI attaches no `network.protocol.version` (or any version key) to HTTP metrics or spans. Cleartext HTTP/2 (h2c) is therefore indistinguishable from HTTP/1.1 in the telemetry — the two produce identically-shaped `http.*` output.
- **gRPC is cleanly separated from plaintext HTTP/2.** OBI detects `content-type: application/grpc` and emits a distinct event type: `rpc.*` metrics (`rpc.server.call.duration` / `rpc.client.call.duration`) and spans named for the full method path, not `http.*`.
- **gRPC attribute keys are the newer semconv v1.41.0 forms:** `rpc.system.name` (=`"grpc"`), `rpc.method` (the **full** path `/pkg.Service/Method` — OBI does not split package/service/method), `rpc.response.status_code`. There is **no** `rpc.service` attribute, and the status key is **not** `rpc.grpc.status_code`.
- **TLS decryption is automatic.** Go `crypto/tls` and OpenSSL (`libssl.so` uprobes) are unconditionally compiled into OBI; there is no enable flag. Decrypted L7 surfaces as ordinary `http.*` / `rpc.*`. BoringSSL is almost always static-linked into the app binary, exposing no `libssl.so` symbol to probe — effectively unsupported.
- **Enablement axes.** `OTEL_EBPF_METRICS_FEATURES` selects telemetry categories (`application` = L7 RED); `OTEL_EBPF_*_INSTRUMENTATIONS` selects protocols (`http`, `grpc`; default `*`). The shipped DaemonSet already runs `application` with default instrumentations, so **no OBI/DaemonSet change is required** for any Phase 3 protocol.

**Decision.**

1. **HTTP/2 rides the existing `protocols.http` toggle; #104 reframed.** Since h2c is indistinguishable from HTTP/1.1, there is no separate `protocols.http2` CRD toggle and no version label. #104's acceptance is re-scoped from "tag `http_version=2`" to "h2c traffic is captured as `http.*` and gRPC-over-HTTP/2 is attributed distinctly as `rpc.*`."
2. **gRPC is its own toggle and its own capture module.** `ProtocolSet.GRPC` (`GRPCConfig` = `enabled` + `ports`, structurally like HTTP but a separate type so the two can diverge later). The reconciler ORs `ProtocolGRPC` (bit `1<<3`; `1<<2` reserved for a hypothetical HTTP/2 that will never be independently selectable) and folds gRPC ports into the same wire port set as HTTP — OBI attaches uprobes per port and discriminates protocol from the wire, not the port, so one port serving both is fine and no new proto field is needed. The agent advertises the `grpc` module in `SupportedModules`.
3. **Translation discriminates by RPC attributes, using the correct keys.** `TranslateTraces` classifies a span as `ModuleGRPC` when `rpc.system.name==\"grpc\"` (with `rpc.system` and bare `rpc.method` fallbacks) and promotes `rpc.method` → `SpanEvent.RPCMethod`, `rpc.response.status_code` → `SpanEvent.RPCStatus`; HTTP fields stay empty for gRPC. `classifyMetric` maps the `rpc.*` metric family → `ModuleGRPC`. The forwarder allowlist (`pkg/schema`) admits `rpc.method`, `rpc.response.status_code`, `rpc.system.name` — all low-cardinality, none sensitive.
4. **TLS gets no toggle; it is a validation + docs deliverable.** #106/#107 add no capture code — TLS-decrypted traffic is already `http.*`/`rpc.*`. They are closed by e2e proof (Go `crypto/tls` server + nginx/OpenSSL) and the protocol matrix documenting automatic decrypt and BoringSSL's limitation.
5. **gRPC contract fixtures start synthetic.** The recorder's workload (agnhost echo) speaks only HTTP, so gRPC is "not yet recordable in our harness." A `-seed` synthetic `grpc-basic`/`grpc-metric-basic` fixture (using the real semconv keys above) provides contract coverage now; real recordings replace it once a gRPC workload is added to the e2e harness (a CI/GKE step, per REGENERATE.md).

**Consequences.**

- ✅ #105 lands as real capture code with the correct semconv keys — dashboards/queries built on it will match what OBI actually emits, avoiding a silent "no data" from querying `rpc.grpc.status_code`/`rpc.service`.
- ✅ No OBI image or DaemonSet change, so Phase 3 carries no re-record-the-world cost and no privilege delta (TLS uprobe caps already shipped in Phase 1).
- ✅ gRPC-vs-HTTP separation is structural (distinct metric names + event type), not heuristic-on-port — robust to mixed-protocol ports.
- ⚠️ Ollie cannot report HTTP protocol version until OBI emits one. If a user needs "HTTP/2 vs HTTP/1.1" breakdown, that is an upstream OBI ask, tracked as roadmap, not a v0.6 deliverable.
- ⚠️ gRPC contract freeze is synthetic until the harness grows a gRPC workload; the unit tests in `pkg/capture` carry the translation guarantee in the interim.
- 📌 BoringSSL users get no L7 decrypt; documented in the matrix as ⚠️ limited.

**Implemented in.** `v0.6/phase-3-protocols`.

---

## Open and superseded ADRs

- **ADR-0004 / ADR-0011** — superseded by ADR-0024 : extensibility moves from an importable Go library + in-process sink interfaces to wire protocols (OTLP push, streaming subscribe, scrape/remote-write). ADR-0004's `pkg/` vs `internal/` layout convention stands.
- **ADR-0017.4** — superseded by ADR-0021. OBI's native K8s attribute attachment is now ON; the agent attaches none.
- **ADR-0020** — sub-decisions superseded in part by ADR-0021. See ADR-0021 consequences for the per-clause status.
- **ADR-0009** — superseded by ADR-0026 §1–2 (was: narrowed by ADR-0022.5). OBI attributes both sides of L4 flows natively at owner granularity (fixture-verified on the pinned image), so the controller→agent identity plane and its fallback informer have no consumer. Reopen condition: a consumer needs L7 metric peer identity or explicit external-peer labeling.

New ADRs are appended above this section.
