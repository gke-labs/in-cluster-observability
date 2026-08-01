---
title: Autoscale on captured traffic
description: Scale a plain HTTP workload with a standard HPA driven by metrics Ollie captures with eBPF — no instrumentation, no exporter, no sidecar.
---

This is the v0.5 capstone in one demo: a standard `autoscaling/v2`
HorizontalPodAutoscaler scales a Deployment on its **request rate as
captured by eBPF** — the workload is not instrumented, exports no
metrics, and has no sidecar. The metric travels: OBI capture → agent
store → query-server PromQL fan-out → `custom.metrics.k8s.io` → HPA
controller.

The runnable manifests live in the repo at
[`examples/hpa/`](https://github.com/gke-labs/in-cluster-observability/tree/main/examples/hpa)
— they are the same manifests the e2e suite scales on every PR, so
they're guaranteed current. The example's README is the canonical
step-by-step; this page is the orientation.

## Prerequisites

Ollie installed per [Getting started](/getting-started/) — the query
server, the custom-metrics APIService, and its RBAC are all part of the
one `kubectl apply -k k8s/`. Verify the aggregator can reach the
backend before starting:

```sh
kubectl get apiservice v1beta1.custom.metrics.k8s.io \
  -o jsonpath='{.status.conditions[?(@.type=="Available")].status}'
# want: True
```

## The shape of the demo

1. **`workload.yaml`** deploys an echo server and a client driving
   ~5 requests/second at it.
2. You read the metric **exactly the way the HPA controller will**,
   through the aggregated API:

   ```sh
   kubectl get --raw \
     '/apis/custom.metrics.k8s.io/v1beta1/namespaces/default/deployments.apps/echo/qps' | jq .
   ```

   `qps` maps to `sum(rate(http_server_request_duration_count{...}[1m]))`
   — a built-in template, overridable via the `ollie-custom-metrics`
   ConfigMap. Built-ins also cover `latency_p50`, `latency_p99`,
   `bytes_in_per_sec`, `bytes_out_per_sec` over pods, deployments,
   statefulsets, and daemonsets. (`services` is not served by default:
   OBI's `service_name` label is OTel service attribution, not the K8s
   Service object — opt in via the ConfigMap if yours line up.)
3. **`hpa.yaml`** targets `500m` qps per replica (an Object metric with
   `AverageValue` semantics: desired replicas = total qps ÷ target).
   Against ~5 rps of traffic, the HPA walks the Deployment from 1 up to
   `maxReplicas: 3` within a couple of minutes; delete the traffic
   client and it walks back down after the HPA's standard 5-minute
   stabilization window.

## Access control, briefly

The kube-apiserver enforces normal RBAC on `custom.metrics.k8s.io`
*before* proxying to Ollie, and Ollie's `:6443` listener requires the
aggregation layer's front-proxy client certificate (mTLS pinned to the
cluster's requestheader CA) — so the metrics are readable through the
K8s API by authorized subjects, and by nobody else, port-open or not.

## Design notes

- A metric with **no data** (idle window, NaN from a quantile over no
  traffic) is answered with `404`, deliberately — the HPA holds its
  current replica count instead of scaling on garbage.
- v0.5 serves **Object metrics on concrete names**. Pods-type metrics
  with label-selector enumeration land with aggregation hints in v0.6.

Full step-by-step, troubleshooting, and cleanup:
[`examples/hpa/README.md`](https://github.com/gke-labs/in-cluster-observability/blob/main/examples/hpa/README.md).
