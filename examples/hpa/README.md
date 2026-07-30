# Autoscale on captured traffic: HPA + Ollie custom metrics

This example scales a plain HTTP workload with a standard
`autoscaling/v2` HorizontalPodAutoscaler driven by **traffic Ollie
captures with eBPF** — the workload is not instrumented, exports no
metrics, and has no sidecar. The metric travels: OBI capture → agent
store → query-server PromQL fan-out → `custom.metrics.k8s.io` → HPA
controller.

These are the same manifests the e2e suite's `TestHPACustomMetrics`
gates on every PR, extracted for standalone use.

## Prerequisites

1. **Ollie installed** — follow
   [Getting started](https://gke-labs.github.io/in-cluster-observability/getting-started/)
   (build/load the three images, `kubectl apply -k k8s/`). All of the
   components this example needs (query server, custom-metrics
   APIService, RBAC) are part of that one install.
2. **`kubectl`** against the same cluster.

Verify the custom-metrics API is up before starting — the aggregator
marks it Available once it can reach the query server:

```sh
kubectl get apiservice v1beta1.custom.metrics.k8s.io \
  -o jsonpath='{.status.conditions[?(@.type=="Available")].status}'
# want: True
```

## 1. Deploy the demo workload

An echo server plus a client driving ~5 requests/second at it:

```sh
kubectl apply -f workload.yaml
kubectl rollout status deployment/echo deployment/traffic-client
```

## 2. See the metric before the HPA does

`qps` is computed over a 1-minute rate window, so give the traffic
about a minute to accumulate, then read the metric exactly the way the
HPA controller will:

```sh
kubectl get --raw \
  '/apis/custom.metrics.k8s.io/v1beta1/namespaces/default/deployments.apps/echo/qps' | jq .
```

You should see a `MetricValueList` with a value around `5` (the demo
client's request rate). Other built-in metrics work the same way:
`latency_p50`, `latency_p99`, `bytes_in_per_sec`, `bytes_out_per_sec`,
over `pods`, `deployments`, `statefulsets`, `daemonsets`, and
`services`. Custom PromQL templates go in the `ollie-custom-metrics`
ConfigMap (`k8s/custommetrics.yaml`).

## 3. Apply the HPA and watch it scale

```sh
kubectl apply -f hpa.yaml
kubectl get hpa echo --watch
```

The target is `500m` qps per replica against ~5 rps of traffic, so
within a couple of minutes the HPA walks `echo` up to `maxReplicas: 3`:

```
NAME   REFERENCE         TARGETS         MINPODS   MAXPODS   REPLICAS
echo   Deployment/echo   4990m/500m      1         3         1
echo   Deployment/echo   4990m/500m      1         3         3
```

To watch it scale back down, delete the traffic source and wait out
the HPA's default 5-minute downscale stabilization window:

```sh
kubectl delete deployment traffic-client
```

## Troubleshooting

- **APIService not Available** — `kubectl -n ollie-system get pods`;
  the `ollie-query` Deployment must be Running. On Kind, remember the
  image-pinning patch from Getting started applies to `ollie-query`
  and `ollie-controller` too, not just the agent DaemonSet.
- **404 "no value for metric"** — traffic hasn't flowed for a full
  minute yet, or the workload isn't actually receiving requests
  (`kubectl logs deploy/traffic-client`). A 404 here is deliberate:
  the adapter refuses to serve a value it doesn't have (including
  NaN from an idle latency quantile), and the HPA holds its current
  replica count.
- **HPA shows `<unknown>`** — check `kubectl describe hpa echo` for
  the controller's fetch error; then read the raw metric as in step 2
  to see the adapter's actual response.

## Cleanup

```sh
kubectl delete -f hpa.yaml -f workload.yaml
```
