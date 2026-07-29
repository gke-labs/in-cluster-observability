# Public Go API

**Status:** Rewritten for [ADR-0024](decisions.md#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157), 2026-07-29 (supersedes the 2026-05-17 embedder-facade draft)
**Owners:** TBD

Per ADR-0024, this project is a **deployable system with open egress**, not an importable library. Extensibility is delivered over the wire — OTLP push, the CEL-filtered streaming subscribe API, the Prometheus scrape endpoint, and remote-write — not through Go interfaces. See [`sinks-and-extensibility.md`](sinks-and-extensibility.md) for the wire surfaces.

This document defines what remains public in Go and the rules that govern it.

## 1. Module layout

A single AP root at the repo root ([ADR-0015](decisions.md#adr-0015-collapse-core-to-repo-root-supersedes-adr-0013-layout)). Go module path: `github.com/gke-labs/in-cluster-observability`.

```
.
├── cmd/
│   ├── ollie/             # agent binary (DaemonSet)
│   ├── ollie-controller/  # control-plane binary (Deployment)
│   └── ollie-query/       # query-server binary (Deployment; v0.5)
├── pkg/                   # public Go surface — see §2
│   ├── capture/           # OBI bridge + OTLP translators
│   ├── controller/        # CRD types, generated stubs, reconcilers
│   └── schema/            # canonical label-key constants
├── internal/              # everything else — no compatibility guarantees
├── images/                # Dockerfiles
├── k8s/                   # install manifests
├── proto/                 # gRPC service definitions (wire contracts)
└── tests/                 # e2e + contract tests
```

**`pkg/` vs `internal/`** (the ADR-0004 layout convention, which ADR-0024 kept): Go's `internal/` mechanism is compiler-enforced, so the boundary is load-bearing. Anything without a demonstrated external consumer lives in `internal/`. Promotion to `pkg/` is an ADR-worthy event; the burden of proof is a named consumer, not an anticipated one.

## 2. The public surface

| Package | What it is | Stability |
|---|---|---|
| `pkg/schema` | Canonical label-key constants + the forwarded-label allowlist | Experimental |
| `pkg/capture` | The OBI bridge; genuinely reusable pieces are `TranslateMetrics` / `TranslateTraces` (OTLP → typed events) and `NewPromMeterProvider` | Experimental |
| `pkg/controller` | CRD Go types (`api/v1alpha1`) and generated gRPC stubs (`pb/`) — public because CRD consumers and codegen need importable paths | Experimental |

Nothing in `pkg/` carries `Stability: Stable`. Per ADR-0024, no package may be tagged Stable until it has **both** an implementation and an external consumer. The v0.5 store and query engines live under `internal/` ([ADR-0025](decisions.md#adr-0025-v05-vertical-slice-implementation-decisions)); if a serious Go embedder ever materializes, a library is extracted from working internals then.

## 3. Stability tags

Every exported symbol in `pkg/*` declares a tier in a `// Stability:` doc comment:

| Tier | Promise |
|---|---|
| **Stable** | Semver: breaking changes are MAJOR bumps. Currently unused — see §2. |
| **Experimental** | May break in MINOR versions; breakages listed in release notes. |
| **Internal** | No guarantees; a symbol exported only for tests inside this module. |

## 4. Wire contracts are the compatibility surface

The promises we do make are protocol-level:

- **OTLP** (receive from OBI; push to operator-configured endpoints): governed by the OTLP spec version pinned in `go.mod`.
- **Prometheus exposition** on the agent `:9090` and the query server's `/api/v1/query*`: standard formats; the metric/label schema is frozen by contract-test goldens (`tests/contract/obi`).
- **gRPC services** under `proto/`: additive evolution only within a major version; field numbers never reused (reserved on removal).
- **CRDs** (`ollie.gke-labs.dev`): `v1alpha1` while shaped; K8s-conventional version migration from `v1beta1` on.

## 5. What `pkg/` is not

- **Not a place to dump utilities.** Shared helpers stay in `internal/`.
- **Not a CLI library.** Tools like `iobsctl` are binaries in `cmd/` speaking the wire APIs.
- **Not a Prometheus / OTel SDK shim.** We expose those ecosystems' own types where they appear; we do not wrap them.
