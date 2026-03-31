# Gemini Coding Guidelines

This document provides pointers and guidelines for coding agents working on this repository.

## Project Structure

- `main.go`: The entry point of the observability agent.
- `go.mod`: Go module definition.

## Coding Standards

- **Go Idioms**: Follow standard Go idioms and best practices.
- **Lightweight**: Keep dependencies to a minimum. We prefer using the standard library or well-established, lightweight packages.
- **Observability**: Ensure all new features include relevant Prometheus metrics.
- **Testing**: Add unit tests for new functionality.

## Using the `ap` tool
The `ap` (autoproject) tool is an automation tool for gke-labs projects, using a "convention over configuration" approach for development tasks. An AP root is marked by a `.ap` directory, and a repository can have multiple AP roots.
If you see `ap build` failing on GitHub, you **must** run `ap build` locally before claiming that it passes.

### How to use `ap`
Since the `ap` tool is not globally installed, run it using `go run github.com/gke-labs/gke-labs-infra/ap@latest [command]`.
Key commands include:
- `ap generate`: Update generated files.
- `ap lint`: Check code style and quality.
- `ap test`: Run unit tests.
- `ap e2e`: Run end-to-end tests.
- `ap build`: Build Docker images from the `images/<image-name>` directory.
- `ap deploy`: Build images and deploy Kubernetes manifests from the `k8s` directory.

### Kubernetes Manifests and Images
- Store manifests in a `k8s` directory within the AP root.
- Do NOT specify `imagePullPolicy` (unless there is a very good reason).
- Image names should map to their directory names under `images/`, with `ap` handling the image prefixing during deployment.
- The build context for the image is the AP root.

## Context for Agents

- This project is intended to run as a Kubernetes `DaemonSet`.
- It interacts with host-level files like `/proc/net/dev` to gather metrics.
- The goal is high performance and low resource overhead.
