# In-Cluster Observability

This project aims to provide lightweight, in-cluster observability for Kubernetes clusters. The primary goal is to explore whether a lightweight monitoring stack, running directly on Kubernetes nodes, can provide enough signal for effective autoscaling and traffic management.

## Current State

The project is in an early "spike" phase. Currently, it includes:
- A Go-based daemonset (conceptually) that gathers network interface metrics from `/proc/net/dev`.
- A Prometheus metrics endpoint (`/metrics`) that exposes these metrics.

## Getting Started

To run the agent locally (on Linux):

```bash
go run main.go
```

Then you can access the metrics at `http://localhost:8080/metrics`.

## Roadmap

- [ ] Deploy as a Kubernetes DaemonSet.
- [ ] Capture more granular network traffic data (e.g., per-pod metrics).
- [ ] Integrate with Kubernetes autoscalers (HPA/VPA).
- [ ] Explore other lightweight metrics sources.