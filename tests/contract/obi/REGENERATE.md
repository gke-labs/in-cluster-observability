# Regenerating contract fixtures

This directory's fixtures freeze the OTLP→`capture.Event` translation. They protect us from silent regressions when the OBI sibling-container image is bumped. Per [`docs/design/obi-integration.md`](../../../docs/design/obi-integration.md) §6.

## Fixture kinds

1. **Recorded real-OBI fixtures** (the committed default since #151): raw OTLP request bodies captured from the pinned OBI image running in the production DaemonSet shape. Provenance (image tag, date, pipeline) is in `testdata/translation/RECORDED.md`. These are what make an OBI image bump a judgeable event.
2. **Synthetic seed fixtures** (bootstrap tool): hand-built OTLP payloads written by `go test ./tests/contract/obi -seed -update`. Use them to bootstrap a case for a protocol OBI can't emit yet in our harness; replace with a recording as soon as one is possible.

The harness treats both the same way — it doesn't know the source.

## Recording real-OBI fixtures (per OBI image bump)

Prerequisites: kind, docker, kubectl — the same set `ap e2e .` needs. The pinned OBI image is read from `k8s/daemonset.yaml` automatically (`otel/ebpf-instrument:<tag>`).

```sh
# 1. Record. Stands up a Kind cluster with the stock k8s/ install,
#    repoints the obi container's OTLP exporter at an in-test recorder
#    on the host, drives HTTP traffic through an agnhost workload, and
#    writes input.binpb + kind + RECORDED.md for each case.
go test ./tests/contract/obi -record -v -timeout 25m

# 2. Regenerate goldens from the new inputs and review the diff.
go test ./tests/contract/obi -update

# 3. Verify.
go test ./tests/contract/obi
```

Commit the new fixtures + goldens **in the same PR as the OBI image-tag bump** (per ADR-0010 / ADR-0018 single-bump-PR policy).

Recorded cases as of #151: `http1-recorded` (HTTP/1.1 server metrics), `l4-recorded` (network flow metrics), `traces-http1-recorded` (HTTP spans). The recorder (`record_test.go`) captures whichever export bodies match each case's classifier, so re-recording after an OBI bump picks up whatever shape the new image emits.

## Synthetic cases pending a real recording

- `grpc-basic` (traces) and `grpc-metric-basic` (metrics) are **synthetic** (`-seed`), added with #105. OBI v0.10.0 emits gRPC fine, but our recorder's workload (`DeployTestWorkload`, agnhost echo) speaks only HTTP — there is no gRPC server + client in the harness yet, so gRPC is "not yet recordable in our harness." The synthetic payloads use OBI's real semconv-v1.41.0 gRPC keys (`rpc.system.name`, `rpc.method` = full path, `rpc.response.status_code`) so the diff against a real recording is small (ADR-0031).
- To replace with real recordings: add a gRPC echo workload + client to the e2e harness, add `grpc-recorded` / `traces-grpc-recorded` classifier entries to `record_test.go` (metric name `rpc.server.call.duration`; span carrying `rpc.system.name`), re-run `-record`, then delete the synthetic `grpc-basic` / `grpc-metric-basic` cases. This must run in CI/GKE (the sandbox cannot build the OBI image or run Kind).

## Adding a new case

- Recorded: extend the `cases` map in `record_test.go` with a classifier, re-run `-record`.
- Synthetic (protocol not yet recordable): extend `fixtures_seed_test.go`, run `-seed -update`.
- Either way, run `-update` and commit `input.binpb`, `kind`, and `golden.json` together.

## When a contract test fails

- **Translation regression**: a code change in `pkg/capture/translate*.go` altered output. Either fix the code or regenerate goldens with `-update` and review the diff.
- **OBI schema change**: a new OBI image emits a different OTLP shape. Re-record per the steps above; if the new shape is intended, regenerate goldens. If unintended, file upstream.
