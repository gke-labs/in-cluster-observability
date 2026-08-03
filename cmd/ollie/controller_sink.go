// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"

	"github.com/gke-labs/in-cluster-observability/pkg/capture"
	"github.com/gke-labs/in-cluster-observability/pkg/controller/agentclient"
	cppb "github.com/gke-labs/in-cluster-observability/pkg/controller/pb/controlplane/v1"
)

// captureSink implements agentclient.Sink. It receives MonitoringSpec
// UPSERT / REMOVE deltas from the controller's gRPC stream and turns
// them into AllowPod / BlockPod calls on the capture.Manager — which
// in turn rewrites OBI's discovery.instrument config (one entry per
// pod, matching by `k8s_pod_name` + `k8s_namespace`) and triggers an
// OBI reload via the existing v0.2 coalescer.
//
// The K8s-metadata match relies on OBI's own informer attaching
// k8s.pod.name / k8s.namespace.name to candidate processes per
// ADR-0021 — there's no PID resolution on the agent side, no
// /proc/<pid>/cgroup parsing, no Kubelet round-trip. The earlier
// pseudo-PID approach (allocate a synthetic uint32 per pod) produced
// dead OBI entries because OBI looked for processes with PIDs the
// host didn't have; the K8s-metadata path lets OBI's discovery loop
// decide which processes belong to which pod.
//
// The protocols bitset on MonitoringSpec is currently advisory —
// OBI's module gating happens at sidecar startup via
// OTEL_EBPF_METRICS_FEATURES; per-pod per-module gating arrives with
// v0.6's richer Module surface.
type captureSink struct {
	mgr capture.Manager
}

func newCaptureSink(mgr capture.Manager) *captureSink {
	return &captureSink{mgr: mgr}
}

func (s *captureSink) OnUpsert(_ context.Context, spec *cppb.MonitoringSpec) error {
	httpPorts := make([]uint16, 0, len(spec.GetHttpPorts()))
	for _, p := range spec.GetHttpPorts() {
		// HttpPorts on the wire is uint32 (proto3 has no uint16); narrow
		// here. Ports above 65535 are nonsense and would clamp via the
		// uint16 cast — defend against that by skipping out-of-range
		// entries.
		if p == 0 || p > 65535 {
			continue
		}
		httpPorts = append(httpPorts, uint16(p))
	}
	return s.mgr.AllowPod(spec.GetPodUid(), capture.PodSpec{
		PodName:   spec.GetPodName(),
		Namespace: spec.GetNamespace(),
		HTTPPorts: httpPorts,
		Labels: map[string]string{
			"k8s.pod.uid":        spec.GetPodUid(),
			"k8s.pod.name":       spec.GetPodName(),
			"k8s.namespace.name": spec.GetNamespace(),
			"ollie.source":       spec.GetSourceRef(),
		},
	})
}

func (s *captureSink) OnRemove(_ context.Context, podUID string) error {
	return s.mgr.BlockPod(podUID)
}

// runControllerClient is called from cmd/ollie's main when
// --controller-addr is set. Blocks until ctx is canceled.
func runControllerClient(ctx context.Context, addr, nodeName string, mgr capture.Manager, logf func(string, ...any)) {
	client, err := agentclient.New(agentclient.Config{
		ControllerAddr:   addr,
		NodeName:         nodeName,
		AgentVersion:     version,
		SupportedModules: []string{"l4_tcp", "http1", "grpc"},
		Sink:             newCaptureSink(mgr),
		Logf:             logf,
	})
	if err != nil {
		logf("controller client init failed: %v", err)
		return
	}
	client.Run(ctx)
}
