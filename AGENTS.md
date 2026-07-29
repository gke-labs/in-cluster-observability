# AGENTS.md

This file documents conventions and operational context for working in this repository. Both humans and agentic coding tools should read it before any non-trivial change. Keep it current as conventions evolve.

`GEMINI.md` is a stub that points back here so the two tools see the same content.

## Current state

This repo is in the middle of a planned rewrite. The legacy POC code (Prometheus + eBPF agent at the root, OpenTelemetry sink/query pipeline under `opentelemetry/`, the `obs/` logging library) was removed early in the rewrite. POC code is preserved on `main` and reachable via `git log main -- <path>`.

All code lives at the **repo root** as a single AP root and single Go module `github.com/gke-labs/in-cluster-observability` (per [ADR-0015](docs/design/decisions.md#adr-0015-collapse-core-to-repo-root-supersedes-adr-0013-layout)). Per [ADR-0018](docs/design/decisions.md#adr-0018-obi-as-sibling-container-not-embedded-library), OBI runs as a **sibling container** in the agent DaemonSet pod, not as an embedded Go library — our agent is an OTLP receiver + OBI config writer.

Milestone status:

- **v0.1 Foundation** ([#64](https://github.com/gke-labs/in-cluster-observability/issues/64)–[#69](https://github.com/gke-labs/in-cluster-observability/issues/69)) landed: AP root, public package skeletons, OBI adapter shell, container image, minimal DaemonSet.
- **v0.2 Capture MVP** ([#70](https://github.com/gke-labs/in-cluster-observability/issues/70)–[#77](https://github.com/gke-labs/in-cluster-observability/issues/77)) landed: OTLP receivers (gRPC + HTTP loopback), OBI config writer, AllowPID/BlockPID with reload coalescer, L4 TCP + HTTP/1.1 translation, OTel self-obs metrics, panic recovery + ModuleDegraded events, debug HTTP endpoint, contract-test harness. DaemonSet now has two containers (obi sibling + agent).
- **v0.3 (lean) — agent + OBI native enrichment** (per [ADR-0021](docs/design/decisions.md#adr-0021-lean-v03--agent-re-uses-obis-native-enrichment), supersedes the original v0.3 "Storage MVP" plan). Adds: OBI v0.9 schema fixes in `internal/obiconfig` (`discovery.instrument` + `open_ports` string + `target_pids`), `--obi-instrument-ports` smoke-test seed, OTel SDK Prometheus exporter always-on with a forwarder that re-records OBI's translated metrics, `--scrape-addr` agent listener at `:9090`, RBAC for OBI's K8s metadata informer, DaemonSet refactor (right caps + `OTEL_EBPF_CONFIG_PATH` env var + `/var/run/obi` + `/sys/fs/cgroup` mounts). What we explicitly did *not* build: a separate `internal/pidcache`, `internal/enricher`, `pkg/sink/promscrape`, or `pkg/store.MetricStore` — OBI's K8s informer + the OTel SDK + this single forwarder replace all of them.
- **v0.4 Control Plane MVP** in progress (per [ADR-0022](docs/design/decisions.md#adr-0022-v04-control-plane-implementation-decisions)). Ships across 4 stacked phase PRs: design refresh + ADR (Phase 0), CRD types + gRPC stubs (Phase 1), reconciler + stream + leader election (Phase 2), RBAC + CR status + AgentStatus (Phase 3). Decisions pinned: API group `ollie.gke-labs.dev`, controller framework `sigs.k8s.io/controller-runtime`, codegen `controller-gen` only, validating webhook deferred to v0.5, identity broadcasting cut (OBI's informer covers source-side per ADR-0021). New binary `cmd/ollie-controller/`; new packages `pkg/controller/{api/v1alpha1,pb,reconciler,stream,agentclient}/`; new manifests `k8s/{controller.yaml,controller-rbac.yaml,crds/}`.
- **v0.4.5 Verification & Soundness** landed (per [ADR-0023](docs/design/decisions.md#adr-0023-verification-first--v045-milestone-gates-v05), issues #150–#158, PRs #160–#167). The OBI boundary is now CI-checkable and the metrics path is sound: `ap-e2e` Kind presubmit exercising the real DaemonSet on every PR; contract fixtures recorded from the pinned OBI image (recorder in `tests/contract/obi/record_test.go`, provenance in `testdata/translation/RECORDED.md`), goldens freeze metric type/temporality/bucket layout; the agent re-emits OBI metrics via a const-metric Prometheus collector (`cmd/ollie/forward.go`) — cumulative totals pass through (OBI exports cumulative; the old forwarder inflated counters), histograms keep buckets, stale series evicted; `pkg/capture` lifecycle bugs fixed; DaemonSet production trim (PSA labels, tolerations, `system-node-critical`, tcpSocket probes, seccomp; CAP_SYS_ADMIN is empirically required by OBI's L7 path on v0.9.0 *and* v0.10.0 — re-attempt each bump); OBI image now `otel/ebpf-instrument:v0.10.0` (no breaking wire changes; additive queue/processing sub-spans).
- **v0.5 Sinks + Query + HPA** in progress, as a vertical slice (tsdb HEAD → PromQL fan-out → custom-metrics API → HPA demo). The #157 gate resolved as [ADR-0024](docs/design/decisions.md#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157): extensibility via wire protocols, not a Go library — `pkg/sink`, `pkg/obsapi`, `pkg/store`, `pkg/query`, and `pkg/topology` are deleted; store/query land under `internal/` shaped by the HPA slice. Implementation decisions pinned in [ADR-0025](docs/design/decisions.md#adr-0025-v05-vertical-slice-implementation-decisions): `tsdb.Open` with ADR-0012 knobs, registry self-scrape ingest, storage-layer fan-out with central PromQL evaluation, separate `cmd/ollie-query` binary, headless-Service agent discovery, hand-rolled minimal custom-metrics API, self-signed TLS with `insecureSkipTLSVerify` until v0.6.

What's in the repo:

- `docs/` — design (`docs/design/`), agreed requirements (`docs/requirements.md`), early rough sketch (`docs/rough_design.md`)
- `AGENTS.md` — this file
- `GEMINI.md` — stub pointing here
- `.ap/` — autoproject config (`ap.yaml`, `headers.yaml`)
- `go.mod` — single Go module
- `cmd/ollie/` — agent binary; OTLP receivers (loopback) + OBI config writer + Prometheus scrape on `:9090` + OBI-metrics re-emission via a const-metric collector (temporality-aware, full histograms, per #153) + optional loopback debug endpoint at `:9099`. v0.4 adds an opt-in `--controller-addr` gRPC client that consumes `MonitoringSpec` deltas from `ollie-controller`.
- `cmd/ollie-controller/` — v0.4 control-plane binary. controller-runtime manager with Lease-based leader election; reconciles TrafficMonitor + ClusterTrafficPolicy + Pod into per-pod `MonitoringSpec`s; gRPC `AgentSession` stream server on `:9102` for agent delivery.
- `pkg/` — public Go surface (all Experimental, per ADR-0024): `capture` (Manager + TranslateMetrics/TranslateTraces + NewPromMeterProvider), `controller` (CRD types + generated stubs + reconcilers), `schema` (label-key constants + forward allowlist)
- `internal/` — private packages: `obiconfig` (typed OBI YAML schema + atomic writer), `otlpreceiver` (loopback gRPC + HTTP OTLP receivers), `debugendpoint` (loopback PID-control HTTP), `scrapeauth` (in-process TokenReview + SubjectAccessReview middleware for the scrape endpoint), `archtest` (enforces OBI import boundary)
- `images/ollie/` — Dockerfile (distroless static, CGO disabled)
- `k8s/` — install manifests (namespace + RBAC + DaemonSet with `obi` + `agent` containers + default-deny NetworkPolicy + kustomization)
- `tests/contract/obi/` — OBI adapter contract tests + fixture harness
- `dev/ci/presubmits/` — CI script wrappers
- `.github/workflows/` — CI YAML
- `LICENSE`, `README.md`, `.gitignore`

## Where to read what

| Topic | Doc |
|---|---|
| What we're building (agreed) | [`docs/requirements.md`](docs/requirements.md) |
| How we're building it | [`docs/design/architecture.md`](docs/design/architecture.md) (entry point) |
| Recorded design decisions | [`docs/design/decisions.md`](docs/design/decisions.md) |
| Per-subsystem design | other files in [`docs/design/`](docs/design/) |
| Roadmap (deferred items) | [`docs/design/roadmap.md`](docs/design/roadmap.md) |
| Issues + milestones | upstream `gke-labs/in-cluster-observability` (see Issue/PR tracking below) |

## Issue and PR tracking

This clone is a fork of `gke-labs/in-cluster-observability`. **All issues and milestones live in upstream**, not the fork. `gh repo set-default` is configured accordingly — `gh issue list`, `gh issue create`, `gh milestone …` target upstream by default. Always link to upstream issue numbers.

### Branch and PR workflow

Two patterns coexist — the right one depends on how stacked the work is.

#### Single-branch milestones (v0.1 → v0.3 used this)

One integration branch per milestone, named after the milestone: `v0.1`, `v0.2`, …, `v1.0`. The branch lived on `upstream` and PR'd to `main` (or to the previous milestone branch before it merged).

This pattern is fine when the milestone's work is small enough to review as a single unit.

#### Phase-stacked milestones (v0.4+ default)

Larger milestones split into stacked **phase branches** under the milestone, each its own PR. Naming: `v0.4/phase-0-design`, `v0.4/phase-1-apis`, `v0.4/phase-2-controller`, `v0.4/phase-3-status`. The slash groups branches together in `git branch` output.

Phase branches live on the personal fork (`origin = mastersingh24/in-cluster-observability`), not on `upstream`, so:

- Force-pushes during iteration don't churn upstream's branch list.
- Each phase PR is a **cross-fork PR** opened against `gke-labs/in-cluster-observability` `main`: `gh pr create --repo gke-labs/in-cluster-observability --base main --head mastersingh24:v0.4/phase-N-...`.
- The phase PR diff includes the prior phases' commits as context until those phases merge to `main` — that's the trade for cleaner upstream branch hygiene.
- When phase N's PR merges, rebase phase N+1 onto the new `upstream/main` (`git rebase upstream/main`, then `git push --force-with-lease origin v0.4/phase-(N+1)-...`). The phase N+1 PR's diff shrinks because phase N's commits are now on main.

```
upstream/main ◄── PR ── mastersingh24:v0.4/phase-0-design   (#first)
              ◄── PR ── mastersingh24:v0.4/phase-1-apis     (rebases as phase-0 merges)
              ◄── PR ── mastersingh24:v0.4/phase-2-controller  (likewise)
              ◄── PR ── mastersingh24:v0.4/phase-3-status     (likewise)
```

Rules (apply to both patterns):

- **Commit fine-grained on the active phase branch.** One logically separable unit per commit. No WIP megacommits.
- **Each phase PR is the review gate** for that phase's work.
- **Never commit directly to `main`.** Main only advances by merging phase PRs (or single-branch milestone PRs).
- **Rebase, don't merge, between phases.** Linear history per phase. (See the `rebase-between-milestones` memory.)
- **Hygiene fixups** for an in-flight phase go on that phase's branch (small commits extending its PR) — not on `main`, not on a later phase branch.

## Build, test, lint

The project uses [`ap`](https://github.com/gke-labs/gke-labs-infra/tree/main/ap) (autoproject). Always invoke via `go run`:

```
go run github.com/gke-labs/gke-labs-infra/ap@latest <command>
```

| Task | Command |
|---|---|
| Generate files | `ap generate //...` |
| Lint | `ap lint //...` |
| Unit + contract tests | `ap test //...` |
| Build images | `ap build //...` |
| E2E (Kind required) | `ap e2e .` |

For quick local iteration, plain Go commands work too: `go build ./...`, `go test ./...`. `ap` is authoritative for any operation that touches generated files, manifests, or images.

CI runs the above via wrappers in `dev/ci/presubmits/`: `ap-build`, `ap-e2e`, `ap-lint`, `ap-test`, `ap-verify-generate`. If `ap build` fails in CI, **run it locally before claiming it passes**.

`ap-verify-generate` fails if `ap generate //...` produces a diff — the hint is the fix.

### Controller codegen (controller-gen + protoc)

The v0.4 controller introduces two artifact families that aren't generated by `ap`:

- DeepCopy + CRD YAML + RBAC YAML from `+kubebuilder:` markers on `pkg/controller/api/v1alpha1/`.
- gRPC Go stubs from `proto/controlplane/v1/*.proto`.

Both are produced by `dev/scripts/codegen.sh`. Versions of `controller-gen`, `protoc-gen-go`, and `protoc-gen-go-grpc` are pinned by blank imports in `tools/tools.go` so `go.mod` is the source of truth; the script invokes them via `go run`. `protoc` itself must be on `$PATH` (v25+; install from [protobuf releases](https://github.com/protocolbuffers/protobuf/releases) for local dev).

Generated outputs are **committed**:

- `pkg/controller/api/v1alpha1/zz_generated.deepcopy.go`
- `k8s/crds/*.yaml`
- `k8s/rbac/controller-generated.yaml`
- `pkg/controller/pb/controlplane/v1/*.pb.go`

Re-run the script after editing the CRD Go types or the proto file:

```sh
./dev/scripts/codegen.sh
git diff   # confirm the regen'd outputs match your edits
```

PR review catches stale codegen for now (no CI verifier yet); a future presubmit may add one.

## Kubernetes manifest conventions (enforced by `ap`)

- Manifests live in `k8s/` at the AP root.
- **Do not set `imagePullPolicy`** unless there's a specific reason — `ap deploy` manages it.
- Image references should be the bare image name (e.g. `ollie`); `ap` adds the registry prefix at deploy.

## Apache 2.0 license headers

Every code/config artifact (Go, YAML, Dockerfile, proto, shell) carries the full Apache 2.0 license header with `Copyright 2026 Google LLC` at the top of the file. Auto-injected for Go and shell by `.ap/headers.yaml`; YAML, Dockerfile, and proto are added by hand (see `.ap/headers.yaml` for the `skip` list). Markdown is unannotated by repo precedent. `go.mod`, `go.sum`, `LICENSE`, and `.git*` files are also skipped.

## Coding conventions

- Standard Go; minimize dependencies (prefer stdlib or established lightweight packages). v0.1 keeps the module stdlib-only; third-party deps land with their consuming milestone.
- Self-observability metric names are prefixed `ollie_<component>_*` — see [`docs/design/operations.md`](docs/design/operations.md) §5. The `pkg/schema` package exports the canonical metric-name and label-key constants; reference those instead of string literals.
- Public Go API surface lives under `pkg/*` with explicit stability tags (`// Stability: Stable | Experimental | Internal`) — see [`docs/design/public-api.md`](docs/design/public-api.md) §3.
- Internal-only code lives under `internal/*` (Go's `internal/` convention enforces this).
- gRPC services proto-defined under `proto/<service>/v<N>/`; generated stubs under `pkg/.../pb/` via `ap generate`.
- eBPF (rare; OBI ships its own): `.bpf.c` files under `internal/bpf/`, bindings via `bpf2go`. Generated files are checked in.

## OBI integration boundary

Per [ADR-0018](docs/design/decisions.md#adr-0018-obi-as-sibling-container-not-embedded-library), OBI runs as a **sibling container** in the agent DaemonSet pod, not as an embedded Go library. The agent is an OTLP receiver that consumes from OBI on localhost.

Consequences for the codebase:

- **No package imports `go.opentelemetry.io/obi/*`.** The boundary is now "zero OBI Go imports anywhere," still enforced by the Go test in [`internal/archtest`](internal/archtest) (see [ADR-0016](docs/design/decisions.md#adr-0016-obi-import-boundary-enforced-via-go-test)).
- **OBI version pinning is image-tag based**, not `go.mod` based. The pin lives in `k8s/daemonset.yaml` and (once v1.0 lands) `helm/ollie/values.yaml`. Bump policy: one tag at a time, dedicated PR, contract tests green.
- **`pkg/capture` is the OBI-bridge package** (OTLP receiver + OBI config writer), not a Go-API wrapper. It exposes the same `Manager` interface from v0.1; the implementation talks OTLP and writes OBI's config file.

See [`docs/design/obi-integration.md`](docs/design/obi-integration.md) for the deployment topology, config flow, reload mechanism, and contract-test fixtures.

## Keeping this file current

This document is expected to drift if not actively maintained. **Edit it in the same PR as any change that affects conventions** — when new packages land, when build commands change, when the install namespace changes, when a milestone PR merges. Agentic coding tools have standing authorization to refresh it when they notice it's out of date.
