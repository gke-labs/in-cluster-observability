# OBI Integration

**Status:** Draft, 2026-05-17 (rewritten for sibling-container model per [ADR-0018](decisions.md#adr-0018-obi-as-sibling-container-not-embedded-library))
**Owners:** TBD

This document specifies how the project consumes OpenTelemetry eBPF Instrumentation (OBI, `go.opentelemetry.io/obi`). The original ADR-0010 anticipated embedding OBI as a Go library; that approach was abandoned when probing OBI v0.9.0 revealed the public Go API (`pkg/ebpf`) is a low-level building-block surface with no documented embedder path, and that OBI's supported deployment model is **as a standalone binary that emits OTLP**.

Under the current model OBI runs as a **sibling container** in the agent DaemonSet pod, configured to push OTLP to our agent on localhost. Our `pkg/capture` package is an OTLP receiver and an OBI-config writer, not a Go-API wrapper.

Background: [ADR-0001](decisions.md#adr-0001-ebpf-data-plane--opentelemetry-ebpf-instrumentation-obi) (OBI is the eBPF library), [ADR-0010](decisions.md#adr-0010-obi-version-pinning-and-adapter) (version pinning principle), [ADR-0018](decisions.md#adr-0018-obi-as-sibling-container-not-embedded-library) (sibling-container shape).

## 1. Goals and non-goals

**Goals:**
- Consume OBI the way its maintainers intend it to be consumed.
- Version-pin OBI so version bumps are bounded, intentional events.
- Keep the OBI image as the sole privileged process; agent runs unprivileged.
- Expose a `pkg/capture.Manager` interface to the rest of the project that hides the sibling-container details — embedders see a uniform "capture" surface whether they run with OBI, a fake, or a future alternative.

**Non-goals:**
- We do NOT embed OBI as a Go library.
- We do NOT vendor OBI's internal packages (`pkg/appolly/instrumenter.go`, etc.).
- We do NOT proxy OBI's full configuration surface — only the subset we need to control per `TrafficMonitor` / `ClusterTrafficPolicy` CRDs.
- We do NOT fork the OBI image except as the absolute last resort (criteria in §7).

## 2. Deployment topology

Each agent DaemonSet pod has two containers:

```
┌──────────────────────── Pod: ollie-agent ────────────────────────┐
│                                                                  │
│  ┌──────────────────┐                  ┌────────────────────┐    │
│  │ container: obi   │                  │ container: agent   │    │
│  │ (upstream image) │ ─── OTLP/gRPC ─► │ (ollie image)      │    │
│  │ CAP_BPF,         │   127.0.0.1:4317 │ unprivileged       │    │
│  │ CAP_PERFMON,     │                  │ runAsNonRoot       │    │
│  │ hostPID,         │                  │                    │    │
│  │ host mounts      │                  │ OTLP receiver →    │    │
│  │                  │                  │   enricher →       │    │
│  │ Watches PIDs,    │                  │   store → sinks    │    │
│  │ attaches eBPF,   │                  │                    │    │
│  │ emits OTLP       │                  │ Writes OBI config  │    │
│  └──────────────────┘                  │ via shared volume  │    │
│         ▲                              └────────────────────┘    │
│         │                                       │                │
│         └────── config reload (SIGHUP) ─────────┘                │
│                                                                  │
│  Shared volume: emptyDir mounted at /etc/ollie/obi-config/       │
│  Host volumes (OBI only): /sys/fs/bpf, /proc, /sys/kernel/debug  │
└──────────────────────────────────────────────────────────────────┘
```

Key properties:

- **Network namespace shared.** OBI pushes OTLP to `127.0.0.1:4317` (gRPC) or `:4318` (HTTP); our agent listens on those ports. No service mesh, no cross-pod traffic.
- **Lifecycle coupled.** Pod restart restarts both containers. If OBI's container dies repeatedly, the whole pod backs off — k8s handles it.
- **Privileges quarantined.** Only `obi` runs with `CAP_BPF` + `CAP_PERFMON` + `CAP_NET_ADMIN` + `hostPID: true` + host mounts (`/sys/fs/bpf`, `/proc` ro, `/sys/kernel/debug` ro). The `agent` container drops all capabilities and runs as `runAsNonRoot: true`, `runAsUser: 65532`.
- **Config flow.** Agent writes OBI's discovery + protocol configuration to a shared `emptyDir` volume mounted at `/etc/ollie/obi-config/`. Agent signals OBI to reload by sending `SIGHUP` via shared process namespace (or via a small reload-signal file plus OBI's config watcher — see §3.4).

The full DaemonSet manifest lands in v0.2 ([#68](https://github.com/gke-labs/in-cluster-observability/issues/68) is updated in v0.2 to add the second container; v0.1 ships agent-only as a stub).

## 3. The `pkg/capture` surface

The `Manager` interface from v0.1 (defined in [`docs/design/data-plane.md`](data-plane.md) and v0.1 commit `b3bd6d8`) is unchanged. Its **implementation** changes.

### 3.1 What backs Manager methods

| Method | Old (embedded) | New (sibling) |
|---|---|---|
| `Start(ctx)` | Load OBI programs via Go API | Start OTLP gRPC + HTTP receivers on `127.0.0.1:4317`/`:4318` |
| `Stop(ctx)` | Detach OBI; close ringbuffer | Drain receivers; close Events channel |
| `AllowPID(pid, spec)` | Call OBI's Go `AllowPID` | Update OBI's discovery config (add PID/selector); signal reload |
| `BlockPID(pid)` | Call OBI's Go `BlockPID` | Update OBI's discovery config (remove); signal reload |
| `EnableModule(mod, cfg)` | Toggle OBI Tracer in the live ProcessTracer | Update OBI's protocol section in config; signal reload |
| `DisableModule(mod)` | Same | Same |
| `Events()` | Channel fed by OBI ringbuffer goroutine | Channel fed by OTLP receiver handlers |
| `AddEnricher(e)` | Unchanged | Unchanged |
| `Metrics()` | Unchanged | Unchanged |

The public surface stays stable — Manager continues to be `// Stability: Experimental` until OBI hits 1.0, but only because OBI itself is unstable, not because our interface is.

### 3.2 OTLP receivers

Two receivers on the agent's loopback:

- `:4317` — OTLP gRPC, served by `go.opentelemetry.io/otel/sdk/...` infrastructure or, more simply, by `google.golang.org/grpc` with handlers registered for `ExportMetricsServiceRequest`, `ExportTraceServiceRequest`, `ExportLogsServiceRequest`.
- `:4318` — OTLP HTTP, served by a small `net/http` mux at `/v1/{traces,metrics,logs}`.

OBI is configured to push to whichever the operator selects (gRPC by default). Agent listens on both so embedders can pick.

### 3.3 Configuring OBI

OBI's config file format is YAML; the relevant subset for our purposes:

```yaml
# /etc/ollie/obi-config/config.yaml (written by agent, read by OBI)
otel_metrics_export:
  endpoint: 127.0.0.1:4317
otel_traces_export:
  endpoint: 127.0.0.1:4317
attributes:
  kubernetes:
    enable: false        # per ADR-0017.4 — we attribute K8s identity ourselves
routes:
  unmatched: wildcard    # leaves path templating to us (per #605, v0.6)
discovery:
  services:              # populated from MonitoringSpec
    - name: <workload>
      open_ports: [...]
      exe_path_regexp: ...
      # OR pid_namespace: ...
```

The agent writes this on `MonitoringSpec` changes from the controller. The controller's per-pod `MonitoringSpec` resolves to a `discovery.services` entry per pod.

### 3.4 Reload mechanism

Two viable options; pick during implementation:

- **SIGHUP:** the agent sends SIGHUP to the OBI process. Requires `shareProcessNamespace: true` on the pod. Cleanest, but `shareProcessNamespace` has security implications worth weighing.
- **Config file watch:** OBI watches its config file (it does, per `pkg/config/...`). Agent writes-then-renames atomically; OBI picks up. No shared process namespace needed.

Defaulting to file-watch reload in v0.2 to avoid `shareProcessNamespace`. SIGHUP is the fallback if OBI's file watcher proves flaky.

## 4. Translating OBI's OTLP → `capture.Event`

OBI emits standard OTLP — `ExportMetricsServiceRequest` and `ExportTraceServiceRequest` messages. Our OTLP receiver translates each to a stream of `capture.Event`s.

Per [ADR-0017.5](decisions.md#175-v02-metricspan-field-set--minimal-http-focused), v0.2 captures the **minimal** field set:

- L4 TCP metrics → `Event{Kind: Metric, Metric: &MetricEvent{name, value, attrs: {peer_ip, peer_port, direction}}}`
- HTTP/1.1 spans → `Event{Kind: Span, Span: &SpanEvent{method, path_raw, status, duration_ns, peer_ip, peer_port}}`
- HTTP/1.1 metrics (counter, histogram) → `Event{Kind: Metric, ...}`
- Per [ADR-0017.4](decisions.md#174-strip-obis-built-in-kubernetes-attribution), v0.2 **dropped OBI's `k8s.*` resource attributes** at translation time. (Superseded by [ADR-0021](decisions.md#adr-0021-lean-v03--agent-re-uses-obis-native-enrichment): OBI's native K8s attribution is now ON and passes through.)

The translation lives in `pkg/capture/otlp_translate.go`; tested via contract tests (§6).

## 5. Per-PID enable / disable

`Manager.AllowPID(pid, spec)`:

1. Update the in-memory `pidSpecs[pid] = spec` (idempotent).
2. Compute the desired `discovery.services` list for the OBI config from all current `pidSpecs`.
3. If the computed list differs from the on-disk OBI config, write a new config file atomically and signal reload.

`Manager.BlockPID(pid)`:

1. Delete from `pidSpecs`.
2. Recompute and write if changed.

Idempotency is free because we always rewrite from the current `pidSpecs` map. Batching: a "reload coalescer" debounces rapid Allow/Block sequences over a 500 ms window so a workload rollout doesn't thrash OBI's reload.

## 6. Contract test suite

Location: `tests/contract/obi/`. Purpose: freeze our **OTLP→Event** translation against pinned inputs so OBI bumps can't silently change our output.

### 6.1 Fixture format

Each test case has:

- An **input fixture** — a recorded OTLP `ExportXxxServiceRequest` payload (binary protobuf) captured from a real OBI run against a known canary workload, stored under `tests/contract/obi/testdata/<case>/input.binpb`.
- A **golden output** — the expected sequence of `capture.Event` (JSON) after translation, stored under `tests/contract/obi/testdata/<case>/golden.json`.

Test driver loads the fixture, feeds it through `pkg/capture`'s translator, captures the actual Event stream, and diffs against the golden. `go test -update` regenerates goldens.

### 6.2 Initial cases (v0.2 #74)

- `l4-basic` — TCP bytes + connection counters for one workload.
- `http1-basic` — HTTP/1.1 request span + counter + histogram.
- `allowpid-lifecycle` — AllowPID then BlockPID; OBI's config changes; agent receives expected events through each phase.
- `module-degraded` — synthetic "OBI container dies" scenario; agent emits `Event{Kind: ModuleDegraded}` after the configured retry budget.

Fixtures are regenerated from a real OBI run on each OBI image bump; the regeneration recipe lives in `tests/contract/obi/REGENERATE.md`.

### 6.3 What contract tests do not cover

- They are **not** an end-to-end test of OBI itself — we trust OBI's upstream test suite for that.
- They are not a kernel-level test. The fixtures abstract over the kernel; the agent doesn't load eBPF.
- E2E tests covering "real OBI + agent + workload" live in `tests/e2e/` (lands with v0.5).

## 7. Version pinning policy

Per [ADR-0010](decisions.md#adr-0010-obi-version-pinning-and-adapter) (principle) + [ADR-0018](decisions.md#adr-0018-obi-as-sibling-container-not-embedded-library) (mechanism):

- Pin a specific OBI image tag in the agent DaemonSet manifest and Helm values (e.g. `ghcr.io/open-telemetry/obi:v0.9.0`).
- Image bumps live in their own PR. The PR is constrained to:
  - `k8s/daemonset.yaml` (image tag bump)
  - `helm/ollie/values.yaml` (when Helm chart lands in v1.0)
  - `tests/contract/obi/testdata/*` (regenerated fixtures)
  - `docs/design/decisions.md` (only if the bump requires a new ADR for behavior changes)
- The PR description must include:
  - OBI release notes summary
  - Any config-shape changes (OBI's config keys we use)
  - Contract-test diff (golden additions / changes)
- Contract test suite must pass without flakiness retries.

If a bump introduces a config-shape change OBI made (e.g. they rename `attributes.kubernetes.enable` to something else), the same PR updates our config-writer. That stays in our repo, but the change is small and bounded.

## 8. Fork-vs-upstream policy

When OBI lacks something we need or has a bug:

1. **First, open an upstream issue.**
2. **Next, contribute a PR upstream.** This is the default.
3. **If urgency demands and upstream is slow:** maintain a downstream image build, applying patches before publishing. Living in a `images/obi-patched/Dockerfile` that pulls upstream, applies the patch, and re-publishes under our registry.
4. **Fork OBI** only if a critical issue sits unaddressed for **two full OBI release cycles** (~two minor releases). A fork is an ADR-worthy decision; the cost (we now own a fork of an active project) is high.

We never silently fork. Path is always: issue → PR → patched image → fork, with explicit gates between each step.

## 9. Failure modes

| Failure | Detection | Response |
|---|---|---|
| OBI container crashes once | k8s container status | Pod restarts the container; agent emits `ollie_capture_obi_restarts_total` |
| OBI container crash-loops | k8s container status; OBI's restart count | Pod backs off (k8s default); agent emits `ModuleDegraded` event and surfaces in pod status |
| OTLP connection refused (OBI not up) | gRPC connect error | Agent retries with backoff; logs at warning; metric ticks |
| OBI config rejected | OBI logs to stderr; non-zero exit | Agent reads OBI's last log lines and surfaces in `AgentStatus` to the controller |
| Loopback OTLP slow | receiver-side latency | Agent buffers; if buffer fills, drops with `ollie_capture_events_dropped_total{reason="backpressure"}` |
| OBI version mismatch with our config writer | OBI rejects unknown keys | Surfaced via OBI's exit; gated by contract tests, should never reach prod |

## 10. Adapter implementation notes

Non-normative; intended to make the first v0.2 PR faster.

- OTLP receivers use `google.golang.org/grpc` + the generated OTLP service stubs (or the OTel SDK's collector exporter as a reference). For v0.2 we go direct gRPC to avoid pulling in the full Collector SDK.
- The reload coalescer is a small goroutine reading from a buffered channel with a 500 ms debounce.
- Config marshaling uses `gopkg.in/yaml.v3` (or `sigs.k8s.io/yaml` if we want JSON compatibility — TBD).
- The `pkg/capture` package gains a `Config.ObiSocketPath` field for the shared volume path; defaults to `/etc/ollie/obi-config/config.yaml`.
- No OBI Go imports anywhere in our module. `internal/archtest` enforces this — the boundary check stays identical because the import path is the same; it's just that nothing imports it now.

## Open questions

1. **Reload mechanism final pick.** File-watch is the v0.2 default; revisit if OBI's watcher proves unreliable in practice.
2. **OBI's discovery config schema stability.** OBI's config evolves between minors; we may want to wrap our writer in a versioned interface so a single switch handles OBI N vs N+1 during transition.
3. **OBI's own `/metrics` endpoint.** OBI exposes Prometheus metrics for its internal state. We should scrape these into our self-observability surface to make OBI's health visible to operators. v0.3 work.
4. **Multi-arch OBI image availability.** Confirm upstream publishes arm64 + amd64 image tags for every minor (per [`testing-and-benchmarks.md`](testing-and-benchmarks.md) §7).
