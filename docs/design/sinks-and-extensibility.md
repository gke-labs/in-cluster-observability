# Sinks and Extensibility

**Status:** Rewritten for [ADR-0024](decisions.md#adr-0024-extensibility-via-wire-protocols-not-a-go-library-resolves-157), 2026-07-29 (supersedes the 2026-05-17 in-process sink-interface draft)
**Owners:** TBD

This document specifies how data leaves the system. Per ADR-0024, a "sink" is not a Go interface — it is a **wire endpoint**. Adding a sink requires no fork and no Go: you point the system at your endpoint, subscribe to a stream, or scrape us. The consumers named in the requirements (existing observability stacks, AI agents, the HPA) all speak these protocols natively.

## 1. The four egress surfaces

| Surface | Direction | Protocol | Ships in |
|---|---|---|---|
| Prometheus scrape | pull | Prometheus text exposition on agent `:9090` | v0.3 ✅ |
| Query API | pull | PromQL over HTTP (`/api/v1/query`, `/api/v1/query_range`) + `custom.metrics.k8s.io` on the query server | v0.5 (vertical slice) |
| OTLP push | push | OTLP/gRPC and OTLP/HTTP to operator-configured endpoints | v0.5 (breadth) — [#97](https://github.com/gke-labs/in-cluster-observability/issues/97), [#98](https://github.com/gke-labs/in-cluster-observability/issues/98) |
| Streaming subscribe | push (subscription) | gRPC server-stream with CEL filter, OTLP-encoded payloads | v0.5 (breadth) — [#99](https://github.com/gke-labs/in-cluster-observability/issues/99) |

Prometheus **remote-write** rides the OTLP push design as a sibling exporter (same queueing/backoff skeleton, different encoder); it shares #97/#98's fate rather than having its own row.

## 2. Prometheus scrape (shipped)

The agent serves the standard text exposition format on `:9090/metrics`: OBI-captured metrics re-emitted through the const-metric collector (temporality-corrected, full histograms — #153) plus `ollie_*` self-observability metrics. Auth is in-process TokenReview/SubjectAccessReview (`internal/scrapeauth`); the `ollie-metrics-reader` ClusterRole grants the nonResourceURL. Label surface is bounded by the `pkg/schema` forward-allowlist (#144), frozen by contract-test goldens.

This is the zero-integration path: point any existing Prometheus (or GMP, or an OTel collector's prometheus receiver) at the DaemonSet.

## 3. Query API (v0.5 vertical slice)

The query server (`cmd/ollie-query`) exposes:

- **`/api/v1/query`, `/api/v1/query_range`** — Prometheus-compatible HTTP API over the cluster-wide fan-out ([ADR-0025](decisions.md#adr-0025-v05-vertical-slice-implementation-decisions) §3, [`storage-and-query.md`](storage-and-query.md) §5). Any Grafana or PromQL client works unmodified. Responses carry `degraded`/`missing_nodes` annotations when agents miss the fan-out deadline.
- **`custom.metrics.k8s.io/v1beta1`** — the aggregated API the HPA consumes ([`storage-and-query.md`](storage-and-query.md) §7). Metric path → PromQL templates come from a ConfigMap; operators add derived metrics by editing it, no recompilation.

## 4. OTLP push (v0.5 breadth)

The agent pushes captured telemetry to operator-configured OTLP endpoints — the front door of every observability vendor and the OTel collector. Configuration is per-endpoint (URL, protocol gRPC|HTTP, headers for auth, compression, timeout); delivery is at-most-once with bounded buffering and exponential backoff. A slow or dead endpoint drops (with `ollie_export_dropped_total` accounting), never blocks capture.

Design details land with #97/#98; the queueing skeleton, self-obs metric names, and backpressure contract from the superseded in-process design carry over (bounded per-endpoint buffer, drop-not-block).

## 5. Streaming subscribe (v0.5 breadth)

A gRPC service on the query server for live consumers (AI agents primarily):

- `Subscribe(filter: CEL, kinds: [SPANS|EDGES|METRICS]) → stream` — long-lived server stream, payloads OTLP-encoded so downstream OTel tooling consumes them directly.
- The query server compiles the CEL program once, fans the subscription out to node-local agents, and multiplexes the results.

Slow consumers get bounded buffering and gap markers, not backpressure into the capture path. `iobsctl` ([#100](https://github.com/gke-labs/in-cluster-observability/issues/100)) is the first-party client of this surface.

## 6. Failure isolation

The invariant the old sink design encoded survives the move to the wire: **egress never takes down capture.**

| Failure | Behavior |
|---|---|
| Push endpoint down / slow | Bounded buffer, exponential backoff, drop after exhaustion; `ollie_export_errors_total` / `..._dropped_total` |
| Scrape client stalls | Standard HTTP write timeout; connection dropped |
| Streaming subscriber stalls | Oldest events dropped per-stream with gap markers; `ollie_stream_dropped_total` |
| Query overload | Engine-level limits (max samples, timeout); `503` beyond concurrency cap |

## 7. Self-observability

Egress metrics, all on the standard self-obs endpoint:

| Metric | Type | Notes |
|---|---|---|
| `ollie_export_batches_total{endpoint}` | counter | OTLP/remote-write push |
| `ollie_export_dropped_total{endpoint,reason}` | counter | reason ∈ {`buffer_full`, `retry_exhausted`} |
| `ollie_export_errors_total{endpoint,kind}` | counter | kind ∈ {`transient`, `permanent`} |
| `ollie_export_duration_seconds{endpoint}` | histogram | per-batch |
| `ollie_stream_subscribers` | gauge | active subscriptions |
| `ollie_stream_dropped_total{reason}` | counter | slow-consumer drops |
| `ollie_query_duration_seconds{api}` | histogram | `promql` \| `custom_metrics` \| `subscribe` |

## Open questions

1. **Remote-write v2.** Start with v1 (universal) and add v2 (native histograms, metadata) when a consumer asks, or lead with v2? Decide in #97/#98 design.
2. **Subscription resume.** Should `Subscribe` support a resume token so a reconnecting AI agent doesn't lose the gap? Defer to the first real consumer; gap markers make the loss visible meanwhile.
3. **Egress config surface.** Push endpoints are operator config — flag/file on the agent vs. a field on `ClusterTrafficPolicy`. Leaning CRD (it's cluster operator intent); decide in #97.
