# Ollie

*Transparent eBPF network observability for Kubernetes workloads.*

Ollie captures L4 and L7 traffic — TCP, HTTP/1.1, HTTP/2, gRPC, and TLS-decrypted L7 — without modifying the workloads themselves, attaches Kubernetes identity to every record, stores recent data in-cluster for low-latency queries (HPA, AI agents), and exports to pluggable long-term sinks via OTLP, Prometheus, or a registerable Go sink interface.

The name plays on **o11y**, the standard observability abbreviation.

## Status

**v0.3 dev preview** — the capture pipeline works end-to-end on real clusters. Deploying the DaemonSet on Kind and pointing nginx traffic at it produces `http_server_request_duration{k8s_pod_name="nginx-...",k8s_namespace_name="...",k8s_deployment_name="..."}` lines on the agent's `:9090/metrics` endpoint, with K8s identity attached automatically by OBI. L4 flows even carry dual-sided attribution (`k8s_src_*` + `k8s_dst_*`) for free.

What's not yet built: CRD-driven onboarding (v0.4), in-cluster `tsdb` HEAD + PromQL (v0.5), `custom.metrics.k8s.io` for HPA (v0.5), AI-agent CEL streaming (v0.5), HTTP/2 + gRPC + TLS (v0.6), Helm chart + operator runbook (v1.0). Today's coverage is L4 TCP + HTTP/1.1.

**Should you try it now?** See the [self-selection matrix](https://gke-labs.github.io/in-cluster-observability/#should-i-try-this-today) on the docs site — three columns covering who benefits today, who should come back at v0.4–v0.5, and who should wait for v1.0.

Milestones are tracked at [github.com/gke-labs/in-cluster-observability/milestones](https://github.com/gke-labs/in-cluster-observability/milestones).

## Try it on Kind

```sh
docker build -t ollie:dev            -f images/ollie/Dockerfile .
docker build -t ollie-controller:dev -f images/ollie-controller/Dockerfile .
docker build -t ollie-query:dev      -f images/ollie-query/Dockerfile .
docker pull otel/ebpf-instrument:v0.10.0
kind create cluster --name ollie-demo
kind load docker-image --name ollie-demo ollie:dev ollie-controller:dev ollie-query:dev otel/ebpf-instrument:v0.10.0
kubectl apply -k k8s/
```

Full walkthrough (including the Kind image-pinning patch, a demo workload, and the scrape recipe) is in the [Getting started](https://gke-labs.github.io/in-cluster-observability/docs/getting-started/) docs. To see an HPA scale an uninstrumented workload on its captured request rate, continue with [`examples/hpa/`](examples/hpa/).

## Where to read what

| You want to | Read |
|---|---|
| Try it on a real cluster | [Getting started](https://gke-labs.github.io/in-cluster-observability/docs/getting-started/) |
| See what's actually shipping | [What works today](https://gke-labs.github.io/in-cluster-observability/docs/what-works-today/) |
| Understand the architecture | [Architecture](https://gke-labs.github.io/in-cluster-observability/docs/architecture/) or [`docs/design/architecture.md`](docs/design/architecture.md) |
| Understand what we're building (full reqs) | [`docs/requirements.md`](docs/requirements.md) |
| Read the decision log | [`docs/design/decisions.md`](docs/design/decisions.md) |
| Contribute | [`AGENTS.md`](AGENTS.md) |
| Browse the roadmap | [`docs/design/roadmap.md`](docs/design/roadmap.md) or the [docs-site roadmap](https://gke-labs.github.io/in-cluster-observability/docs/roadmap/) |

A polished user-facing README ships with v1.0 ([issue #113](https://github.com/gke-labs/in-cluster-observability/issues/113)). This version is the in-flight pointer.

## Naming

The project is **Ollie** — capitalized in prose, lowercase as the binary / image / metric prefix (`ollie`, `ollie:v0.3`, `ollie_capture_events_total`). The repository name is the more descriptive `gke-labs/in-cluster-observability`; the Go module path matches the repository (`github.com/gke-labs/in-cluster-observability`). The name plays on **o11y**, the standard observability abbreviation.

## License

Apache 2.0 — see [`LICENSE`](LICENSE).

## Disclaimer

This is not an officially supported Google product.

This project is not eligible for the Google Open Source Software Vulnerability Rewards Program.
