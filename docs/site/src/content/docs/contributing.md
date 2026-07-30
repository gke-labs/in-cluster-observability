---
title: Contributing
description: How to file issues, structure PRs, and work on Ollie.
---

The canonical contributor reference is
[`AGENTS.md`](https://github.com/gke-labs/in-cluster-observability/blob/main/AGENTS.md)
in the repo root — it covers conventions, build commands, layout, and
the OBI integration boundary. This page is a short pointer.

## Filing issues

All issues and milestones live in upstream
`gke-labs/in-cluster-observability`. Even if you're working on a fork,
file issues against upstream — the fork's issue tracker is
intentionally empty.

## Branch and PR model

Work lands on `main` through **phase branches**: a milestone's work is
split into phases, each phase branch PRs to `main` and squash-merges
once CI is green. Within a phase branch, commits are **fine-grained** —
one logically separable unit per commit, no WIP megacommits.
Significant decisions are recorded as ADRs in
[`docs/design/decisions.md`](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/design/decisions.md)
in the same PR as the code they explain.

## Build and test

The project uses
[`ap`](https://github.com/gke-labs/gke-labs-infra/tree/main/ap)
(autoproject):

```sh
go run github.com/gke-labs/gke-labs-infra/ap@latest test //...
go run github.com/gke-labs/gke-labs-infra/ap@latest lint //...
go run github.com/gke-labs/gke-labs-infra/ap@latest build //...
```

For quick local iteration, plain Go commands work too:

```sh
go build ./...
go test ./...
```

CI runs `ap` via wrappers in `dev/ci/presubmits/`, plus a Kind e2e
suite (`tests/e2e`, `RUN_E2E=1`) that exercises the real DaemonSet,
query server, and HPA path on every PR. If a presubmit fails in CI,
**reproduce it locally before claiming it passes**.

## Apache 2.0 license header

Every code/config artifact (Go, YAML, Dockerfile, proto, shell) carries
the full Apache 2.0 license header with `Copyright 2026 Google LLC`.
Auto-injected for Go and shell; YAML, Dockerfile, and proto get it by
hand. Markdown is unannotated by repo precedent. `go.mod`, `go.sum`,
`LICENSE`, and `.git*` files are skipped.

## OBI integration boundary

Per ADR-0018, OBI runs as a **sibling container**, not an embedded Go
library. The boundary is enforced by a Go test in `internal/archtest`:
**no package imports `go.opentelemetry.io/obi/*`**. OBI version pinning
is image-tag-based, lives in `k8s/daemonset.yaml`, and is bumped one
tag at a time in dedicated PRs with the contract tests green.

## See also

- [`AGENTS.md`](https://github.com/gke-labs/in-cluster-observability/blob/main/AGENTS.md)
  — the full conventions doc.
- [`docs/design/decisions.md`](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/design/decisions.md)
  — every architectural decision recorded as an ADR.
- [Upstream issues](https://github.com/gke-labs/in-cluster-observability/issues)
  — file bugs, request features, browse milestones.
