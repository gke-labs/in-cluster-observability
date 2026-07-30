---
title: Roadmap
description: What's shipped through v0.5.1, what v0.6 hardening covers, and what's deferred to GA.
---

The project is on a milestone cadence — `v0.1` → … → `v1.0`. Issue and
milestone numbers link to upstream
[`gke-labs/in-cluster-observability`](https://github.com/gke-labs/in-cluster-observability/milestones).

## Shipped

### v0.1–v0.2 — foundation and capture MVP

Single Go module, distroless agent image, minimal DaemonSet (v0.1);
OTLP receivers on loopback, atomic OBI config writer, L4 + HTTP/1.1
translation, self-obs metrics, the two-container pod (v0.2).

### v0.3 (lean) — agent + OBI native enrichment

Reframed by ADR-0021: smoke testing showed OBI's informer already
produces a superset of the K8s labels we'd planned to attach ourselves.
What landed: the OBI config schema in `internal/obiconfig`, the
`--obi-instrument-ports` seed, attribute passthrough, the always-on
`:9090` Prometheus endpoint, the production capability set, and
`ollie_agent_up`.

### v0.4 — control plane MVP

`TrafficMonitor` + `ClusterTrafficPolicy` CRDs, the controller
Deployment with Lease-based leader election, the controller↔agent gRPC
stream, reconciliation into per-node monitoring specs, and CR status
reporting. (The validating webhook moved to v0.6 with the CA machinery
it needs.)

### v0.4.5 — verification and soundness

The gate before building consumers on the metrics (ADR-0023): a Kind
e2e presubmit on every PR, contract fixtures recorded from the pinned
OBI image, a temporality-sound const-metric forwarder (full histograms,
no counter inflation), DaemonSet production trim, OBI v0.10.0.

### v0.5 — sinks, query, HPA

The milestone that closed the loop (ADR-0024/0025/0026):

- Embedded Prometheus tsdb per node + remote read (`:9091`).
- Stateless query server: storage-layer PromQL fan-out with degraded
  semantics, `/api/v1/query` on `:9095`.
- `custom.metrics.k8s.io` for the HPA — an HPA scales a live workload
  on captured metrics in e2e.
- Spans-only raw-OTLP ring + CEL-at-source streaming subscribe.
- OTLP relay + Prometheus remote-write push egress.
- `iobsctl` CLI over the public surfaces only.
- Scope cuts recorded on fixture evidence: the identity-broadcast plane
  (#101–#103) closed as superseded by OBI's native dual-sided
  attribution; extensibility settled as wire protocols, not a Go
  library (ADR-0024).

### v0.5.1 — hardening from adversarial review

An adversarial multi-agent review of the merged v0.5 tree found what
green CI had masked
([ADR-0027](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/design/decisions.md#adr-0027-v051-hardening--adversarial-review-closes-real-defects-the-green-ci-self-merge-masked)):
an unauthenticated custom-metrics port and six correctness bugs. All
fixed with regression tests: mTLS front-proxy client-cert auth on
`:6443`, concurrent fan-out that degrades instead of aborting,
node-identity labels on self-obs series, NaN/Inf guarded out of the
HPA path, forwarder label schemas that widen instead of freezing, and
L4 `direction` preserved as a label.

## Next

### v0.6 — hardening

[Milestone](https://github.com/gke-labs/in-cluster-observability/milestone/6).
Two threads:

**Protocol + TLS coverage:** HTTP/2 and gRPC capture modules, TLS
uprobes (Go `crypto/tls`, OpenSSL), serving-cert CA bundles for the
APIService, TLS on agent↔query hops, the validating webhook, path
templating, sampling, cardinality caps, aggregation hints (Pods-type
HPA metrics).

**The ADR-0027 backlog** (defects known and deferred): honor the HPA's
`metricLabelSelector`, isolate CEL runtime errors per span, flip the
span ring to drop-oldest, fix the false-green `iobsctl` e2e assertion,
raise the agent memory limit to the sized 400Mi, staleness markers,
readiness gating on the remote-read port, and the docs-site migration
([#182](https://github.com/gke-labs/in-cluster-observability/issues/182)).

### v1.0 — GA

[Milestone](https://github.com/gke-labs/in-cluster-observability/milestone/7).
Helm chart, full user-facing README, operator runbook, upgrade guide,
bundled Grafana dashboard, performance budgets in CI, kernel-matrix CI,
multi-arch images.

## Beyond v1.0

Kafka protocol parsing, additional TLS library uprobes (rustls, NSS,
JSSE), multi-cluster federation, a first-party UI, long-term storage
adapters beyond OTLP/Prometheus.

## See also

- [`docs/requirements.md`](https://github.com/gke-labs/in-cluster-observability/blob/main/docs/requirements.md)
  — the agreed requirements catalog.
- [Upstream milestones](https://github.com/gke-labs/in-cluster-observability/milestones)
  — live status.
