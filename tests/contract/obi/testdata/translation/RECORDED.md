# Recorded fixture provenance

- OBI image: otel/ebpf-instrument:v0.9.0
- Recorded: 2026-07-28
- Pipeline: TestRecordFixtures (go test ./tests/contract/obi -record) —
  stock k8s/ install on Kind, obi container's OTEL_EXPORTER_OTLP_ENDPOINT
  repointed at an in-test recorder on the host; agnhost echo workload on
  port 8080 with a wget loop client (tests/e2e harness).
- Regenerate: see REGENERATE.md. Re-record on every OBI image bump and
  regenerate goldens with -update in the same PR (ADR-0010 / ADR-0018).
