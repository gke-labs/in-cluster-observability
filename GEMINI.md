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

## Context for Agents

- This project is intended to run as a Kubernetes `DaemonSet`.
- It interacts with host-level files like `/proc/net/dev` to gather metrics.
- The goal is high performance and low resource overhead.
