#!/bin/bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Regenerate Ollie's controller artifacts:
#   - DeepCopy methods + CRD YAML + RBAC YAML from the
#     +kubebuilder: markers in pkg/controller/api/v1alpha1/.
#   - Go message + gRPC service stubs from
#     proto/controlplane/v1/*.proto.
#
# Versions are pinned by tools/tools.go (so go.mod is the source of
# truth) and resolved at run time via `go run`.
#
# Prereqs:
#   - Go 1.25+ (matches go.mod)
#   - protoc on PATH (v25+). Install: see
#     https://github.com/protocolbuffers/protobuf/releases
#
# Run from the repo root:
#   ./dev/scripts/codegen.sh

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"

echo "==> controller-gen: deepcopy methods (pkg/controller/api/v1alpha1)"
go run sigs.k8s.io/controller-tools/cmd/controller-gen \
  object:headerFile=dev/scripts/boilerplate.go.txt \
  paths=./pkg/controller/api/v1alpha1/...

echo "==> controller-gen: CRD YAML (k8s/crds/)"
mkdir -p k8s/crds
go run sigs.k8s.io/controller-tools/cmd/controller-gen \
  crd \
  paths=./pkg/controller/api/v1alpha1/... \
  output:crd:dir=k8s/crds

echo "==> controller-gen: RBAC YAML (k8s/rbac/controller-generated.yaml)"
# The +kubebuilder:rbac: markers land in v0.4 Phase 3 alongside the
# reconciler. Phase 1 ships an empty placeholder so the
# `ap generate //...` invocation has a stable output path.
mkdir -p k8s/rbac
go run sigs.k8s.io/controller-tools/cmd/controller-gen \
  rbac:roleName=ollie-controller \
  paths=./pkg/controller/... \
  output:rbac:dir=k8s/rbac \
  >/dev/null 2>&1 || true

echo "==> protoc: gRPC stubs (proto/controlplane/v1/ -> pkg/controller/pb/)"
if ! command -v protoc >/dev/null 2>&1; then
  echo "  ! protoc not on PATH; skipping (install: https://github.com/protocolbuffers/protobuf/releases)" >&2
  exit 0
fi
# Plugins resolved by `go run` so versions match tools/tools.go.
PROTOC_GEN_GO_PATH="$(go env GOPATH)/bin/protoc-gen-go"
PROTOC_GEN_GO_GRPC_PATH="$(go env GOPATH)/bin/protoc-gen-go-grpc"
if [[ ! -x "${PROTOC_GEN_GO_PATH}" ]]; then
  go install google.golang.org/protobuf/cmd/protoc-gen-go
fi
if [[ ! -x "${PROTOC_GEN_GO_GRPC_PATH}" ]]; then
  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc
fi
PATH="$(go env GOPATH)/bin:${PATH}" \
protoc \
  --proto_path=proto \
  --go_out=pkg/controller/pb \
  --go_opt=paths=source_relative \
  --go-grpc_out=pkg/controller/pb \
  --go-grpc_opt=paths=source_relative \
  proto/controlplane/v1/controlplane.proto

echo "==> protoc: gRPC stubs (proto/stream/v1/ -> pkg/stream/pb/)"
mkdir -p pkg/stream/pb
PATH="$(go env GOPATH)/bin:${PATH}" \
protoc \
  --proto_path=proto \
  --go_out=pkg/stream/pb \
  --go_opt=paths=source_relative \
  --go-grpc_out=pkg/stream/pb \
  --go-grpc_opt=paths=source_relative \
  proto/stream/v1/stream.proto

echo "==> done. Run 'go test ./...' to verify."
