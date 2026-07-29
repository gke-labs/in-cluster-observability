# Public Go API

**Status:** Draft, 2026-05-17 — **superseded in substance by [ADR-0024](decisions.md#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157) (2026-07-29):** extensibility is wire-protocol-based; the public Go surface shrinks to `pkg/schema` + the OTLP translators (Experimental). This document is retained until the v0.5 rewrite; treat its embedder narrative as historical.
**Owners:** TBD

This document defines the **library surface** that third-party integrators import. Per [ADR-0004](decisions.md#adr-0004-library--controller-posture-public-api-in-pkg) and requirement §2.4, "third parties wrap us and register their own sinks" is load-bearing — every other design doc assumes the API contract described here.

The default `ollie` controller binary is itself an embedder: it imports the same packages a third party would, and registers the built-in sinks. There is no "internal-only" path.

## 1. Module layout

A single AP root at the repo root ([ADR-0015](decisions.md#adr-0015-collapse-core-to-repo-root-supersedes-adr-0013-layout)). Go module path: `github.com/gke-labs/in-cluster-observability`.

```
.
├── .ap/
├── cmd/
│   └── ollie/         # default controller binary
│       └── main.go
├── pkg/                       # public — semver-governed (see §3)
│   ├── capture/               # OBI adapter; eBPF tracer lifecycle
│   ├── store/                 # in-cluster store (tsdb + ring buffer)
│   ├── query/                 # PromQL + CEL query engines
│   ├── sink/                  # PushSink / PullSink / StreamingSink
│   ├── topology/              # K8s identity resolution + caches
│   ├── controller/            # CRD reconcilers, identity broadcaster
│   ├── schema/                # OTel semconv shims, label/attr keys
│   └── obsapi/                # the public "embed me" facade (see §2)
├── internal/                  # private — no compatibility guarantees
│   ├── bpf/                   # any custom .bpf.c we add (rare)
│   ├── enricher/              # per-record attribute application
│   ├── wal/                   # WAL helpers, snapshot orchestration
│   └── grpcwire/              # controller↔agent gRPC service impls
├── images/                    # Dockerfiles (default binary + side tools)
├── k8s/                       # default install manifests
├── tests/
│   ├── e2e/                   # Kind-based; reuses harness pattern
│   ├── bench/                 # canary workload bench harness
│   └── contract/              # OBI adapter contract tests
└── go.mod
```

**Why `pkg/` and `internal/`.** Go's `internal/` convention is enforced by the compiler: nothing outside the module can import `internal/*`. This makes the boundary load-bearing, not aspirational.

**Why `pkg/obsapi`.** It's the **one-stop facade** for embedders who don't want to wire seven packages by hand. It composes the rest and exposes a single `App` type. Power users still reach into the sub-packages.

## 2. The embedder's wiring story

A complete third-party binary that captures HTTP/gRPC from selected workloads, stores locally, and pushes everything to a Webhook URL. Roughly 40 lines of meaningful code:

```go
// cmd/my-obs/main.go
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"

    "github.com/gke-labs/in-cluster-observability/pkg/obsapi"
    "github.com/gke-labs/in-cluster-observability/pkg/sink"

    // built-in sinks the embedder wants
    otlpsink "github.com/gke-labs/in-cluster-observability/pkg/sink/otlp"

    // their own sink
    "example.com/my-webhook-sink/webhook"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    app, err := obsapi.New(obsapi.Config{
        Role:       obsapi.RoleAgent, // RoleAgent | RoleController | RoleQuery | RoleAll
        Namespace:  "ollie-system",
        StorePath:  "/var/lib/ollie",
        Retention:  10 * time.Minute,
    })
    if err != nil {
        log.Fatal(err)
    }

    // Register the user's sink alongside built-ins.
    app.Sinks().Register(otlpsink.New(otlpsink.Config{Endpoint: "otel-collector:4317"}))
    app.Sinks().Register(webhook.New("https://hooks.example.com/obs"))

    // Optional: install an enrichment hook to attach a "tenant" attribute.
    app.Capture().AddEnricher(func(ctx context.Context, r *sink.Record) {
        r.Attributes["tenant"] = lookupTenant(r.Source.Namespace)
    })

    if err := app.Run(ctx); err != nil {
        log.Fatal(err)
    }
}
```

What's happening:
1. **`obsapi.New`** constructs and wires `capture`, `store`, `query`, `topology`, and the controller/agent gRPC clients per the chosen `Role`. Sensible defaults; everything overridable.
2. **`app.Sinks().Register(...)`** adds sinks. The order doesn't matter; sinks are evaluated per record per their type ([`sinks-and-extensibility.md`](sinks-and-extensibility.md)).
3. **`app.Capture().AddEnricher(...)`** is an example of the hook surface (§4 below) — embedders can mutate records before they hit the store or sinks.
4. **`app.Run(ctx)`** starts everything and blocks until `ctx` is done. On cancel: drain push sinks, snapshot WAL, close.

That's it. Embedders who need more control instantiate `capture.New`, `store.New`, etc. directly; `obsapi` is the shortcut, not a wall.

## 3. Stability tiers

Every exported symbol in `pkg/*` has one of three stability tags, declared in a `// Stability:` doc comment immediately above the declaration.

| Tier | Promise | Examples |
|---|---|---|
| **Stable** | Semver. Breaking changes are MAJOR-version bumps. | `obsapi.App`, `sink.PushSink`, `sink.Lifecycle`, `topology.Identity`, the OBI-adapter `capture.Tracer` interface |
| **Experimental** | May break in MINOR versions. Must declare a `// Stability: Experimental` comment. Documented in release notes. | New protocol-module APIs while protocol coverage is being expanded |
| **Internal** | No guarantees. Use only inside this module. The Go `internal/` mechanism enforces this for whole packages; for single symbols inside a public package, use `// Stability: Internal` and document. | (rare) helper exposed for tests only |

A package is "Stable" when **every** exported symbol in it is Stable. `pkg/sink` is Stable from day one; `pkg/capture` starts Experimental because of OBI v0 churn (see [`obi-integration.md`](obi-integration.md)) and graduates to Stable once OBI reaches v1.0.

The release notes for every MINOR release list every Experimental → Stable graduation and every Experimental breakage.

## 4. Hook surface

Embedders extend behavior via hooks rather than subclassing. Hooks are registered through methods on `obsapi.App` (or on the underlying packages for power users).

| Hook | Signature | Purpose |
|---|---|---|
| Enricher | `func(ctx, *Record)` | Mutate records (add attributes, normalize fields) before store + sinks |
| Sample decision | `func(ctx, *RecordPreview) bool` | Drop records before any work happens; called in the hot path |
| Identity override | `func(ip net.IP) (topology.Identity, bool)` | Provide custom IP→identity resolution (e.g. for service meshes with overlay IPs) |
| Sink lifecycle | `func(sink.Sink, sink.Event)` | Observe sink Start/Stop/Error events for embedder metrics |

Hooks fire in registration order. They are called synchronously in the hot path; expensive work must be deferred to background goroutines by the hook itself.

## 5. Role configuration

`obsapi.Role` declares which subsystems an embedder runs. The default binary uses `RoleAll` in single-node test clusters; production deployments split:

| Role | Runs | Typical deployment |
|---|---|---|
| `RoleAgent` | capture + local store + push sinks | DaemonSet |
| `RoleController` | CRD reconcilers, identity broadcaster, validating webhook | Deployment (HA) |
| `RoleQuery` | query server, pull/streaming sinks, custom-metrics-API | Deployment |
| `RoleAll` | all of the above in one process | dev / single-node |

Roles are not mutually exclusive; an embedder can specify `RoleAgent | RoleQuery` to combine. `obsapi` validates the combination and refuses incoherent ones (e.g. `RoleController` without K8s API access).

## 6. Versioning and compatibility

- **Module semver.** This module follows [Go module semver](https://go.dev/ref/mod#versions). Until 1.0.0, breaking changes are allowed in MINOR; from 1.0.0, only MAJOR.
- **Deprecation policy.** Stable APIs marked deprecated stay for two MAJOR versions (so a v1.x deprecation removes at v3.0).
- **gRPC services.** The controller↔agent wire protocol uses protobuf with `option (versioning).stable = true` on stable methods. Backward-compatible additions only.
- **CRD versions.** `v1alpha1` while shaped; `v1beta1` once schema is frozen; `v1` once we ship. Storage version migration handled by the controller per K8s conventions.
- **OBI bumps.** Insulated by the adapter ([`obi-integration.md`](obi-integration.md)); embedders do not see OBI types.

## 7. Stable types — sketches

Sketches only — full reference doc per package. These define the **shape** of the contract.

```go
// Package obsapi — the one-stop embedder facade.
package obsapi

// Stability: Stable
type App interface {
    Capture() *capture.Manager
    Store()   *store.Store
    Query()   *query.Engine
    Sinks()   *sink.Registry
    Topology() *topology.Resolver
    Run(ctx context.Context) error
}

// Stability: Stable
type Config struct {
    Role       Role
    Namespace  string
    StorePath  string
    Retention  time.Duration
    Logger     logr.Logger      // optional; defaults to obs.DefaultLogger
    Tracer     trace.Tracer     // optional; defaults to noop
    // …other tunables with defaults…
}

// Stability: Stable
type Role uint32
const (
    RoleAgent      Role = 1 << iota
    RoleController
    RoleQuery
    RoleAll = RoleAgent | RoleController | RoleQuery
)

func New(Config) (App, error)
```

```go
// Package sink — the registration surface.
package sink

// Stability: Stable
type Lifecycle interface {
    Init(ctx context.Context, deps Deps) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Name() string
}

type Deps struct {
    Logger  logr.Logger
    Store   StoreReader   // read-only view of the in-cluster store
    Metrics metric.Meter  // self-observability handle
}

// Stability: Stable
type PushSink interface {
    Lifecycle
    Write(ctx context.Context, batch Batch) error
}

// Stability: Stable
type PullSink interface {
    Lifecycle
    RegisterRoutes(mux *http.ServeMux)
}

// Stability: Stable
type StreamingSink interface {
    Lifecycle
    Subscribe(ctx context.Context, filter string) (<-chan Event, error)
}

// Stability: Stable
type Registry interface {
    Register(s Sink) error            // s implements one or more of the three interfaces
    Unregister(name string) error
    List() []Sink
}
```

```go
// Package topology — identity types.
package topology

// Stability: Stable
type Identity struct {
    Kind       Kind         // Pod, Service, Node, ExternalIP, Unknown
    Namespace  string
    Name       string
    Labels     map[string]string
    Owner      *OwnerRef    // workload owner (Deployment, StatefulSet, …) if resolvable
}
```

Full per-package references live alongside each package's design doc (e.g. [`storage-and-query.md`](storage-and-query.md) for `store` and `query`).

## 8. What `pkg/` is not

- **Not a place to dump utilities.** Anything that doesn't have a real third-party use case stays in `internal/`. Promotion is an ADR-worthy event.
- **Not a CLI library.** `otelctl`-style tools are separate binaries in `cmd/`; they import `pkg/` like any other consumer.
- **Not a Prometheus / OTel SDK shim.** We re-export `prometheus/client_golang` types where it would otherwise leak; we do **not** wrap them with our own.

## Open questions

1. **Plugin loader vs. compile-time embedding.** All examples above assume the embedder builds a binary. Do we want a Go-plugin-style dynamic loader for sinks? Likely no — Go plugins are operationally painful — but worth a final call before locking the interface.
2. **Multi-tenant API surface.** Hooks let one embedder rewrite records, but if a single binary serves multiple "tenants" each registering disjoint sinks, do we need a `TenantID` namespace on the registry? Out of scope for v1; revisit if a real consumer demands it.
3. **WASM filters.** A long-term option for letting non-Go users provide filter/enrichment logic. Not for v1.
