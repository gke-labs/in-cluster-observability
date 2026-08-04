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

// Package reconciler hosts the controller-runtime reconcilers for
// TrafficMonitor + ClusterTrafficPolicy + Pod, plus the shared
// "compute coverage" engine they all funnel into. v0.4 MVP uses a
// full-recompute-on-any-event strategy — simple and correct.
// Targeted incremental reconciliation lands when scale requires it.
package reconciler

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
	cppb "github.com/gke-labs/in-cluster-observability/pkg/controller/pb/controlplane/v1"
)

// Dispatcher is the subset of *stream.Dispatcher the engine needs.
// Defining the interface here keeps the package free of an import
// cycle on the stream package (which itself imports nothing from
// reconciler).
type Dispatcher interface {
	Apply(map[string]*cppb.MonitoringSpec)
}

// Engine is the shared "compute coverage and dispatch deltas"
// machine. All three reconcilers (TrafficMonitor, ClusterTrafficPolicy,
// Pod) call Engine.Recompute on every reconcile event.
type Engine struct {
	Client     client.Client
	Dispatcher Dispatcher

	// Agents is the per-node agent-status reporter (Phase 3 #93).
	// May be nil — status writes then report 0 actively-monitored.
	Agents AgentReporter

	mu sync.Mutex // serializes Recompute; controller-runtime can
	// dispatch up to N reconciles in parallel via the manager's
	// MaxConcurrentReconciles knob, but the recompute step is a
	// shared mutation point on the dispatcher's internal map.
}

// Recompute lists every Pod, TrafficMonitor, and ClusterTrafficPolicy
// in the cluster, calls ComputeCoverage to produce the per-pod specs
// (and CR status rollups), and hands the resulting pod_uid →
// MonitoringSpec map to the Dispatcher. Pods
// whose spec is nil (no covering CR or no enabled protocol) are
// omitted; Dispatcher emits REMOVE deltas for pods absent from the
// new map.
//
// Pods without a NodeName (still being scheduled) are skipped — the
// agent dispatch key is the node name, so unscheduled pods can't be
// routed anywhere yet. They'll appear in the next reconcile after
// the scheduler binds them.
func (e *Engine) Recompute(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var podList corev1.PodList
	if err := e.Client.List(ctx, &podList); err != nil {
		return err
	}
	var tmList v1alpha1.TrafficMonitorList
	if err := e.Client.List(ctx, &tmList); err != nil {
		return err
	}
	var ctpList v1alpha1.ClusterTrafficPolicyList
	if err := e.Client.List(ctx, &ctpList); err != nil {
		return err
	}

	tms := make([]*v1alpha1.TrafficMonitor, 0, len(tmList.Items))
	for i := range tmList.Items {
		tms = append(tms, &tmList.Items[i])
	}
	ctps := make([]*v1alpha1.ClusterTrafficPolicy, 0, len(ctpList.Items))
	for i := range ctpList.Items {
		ctps = append(ctps, &ctpList.Items[i])
	}
	pods := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, &podList.Items[i])
	}

	// One pass produces both the dispatcher input and the per-CR
	// status rollups (covered pods, conflicts).
	cov := ComputeCoverage(pods, tms, ctps)
	e.Dispatcher.Apply(cov.Specs)

	// Phase 3: write status back. Errors are non-fatal — a failed
	// status patch shouldn't block the dispatch path; controller-
	// runtime will retry on the next reconcile.
	if err := WriteStatuses(ctx, e.Client, tms, ctps, cov, e.Agents); err != nil {
		// Surface the error so controller-runtime's rate-limited
		// retry kicks in, but the deltas have already been
		// applied so the data plane is correct.
		return err
	}
	return nil
}

// TrafficMonitorReconciler is the canonical controller-runtime
// reconciler for TrafficMonitor. Its Reconcile is a thin shell that
// triggers a global recompute via the shared Engine.
type TrafficMonitorReconciler struct {
	Engine *Engine
}

// Reconcile satisfies reconcile.Reconciler. Always returns Result{}
// (no requeue on success); errors get the standard controller-runtime
// rate-limited retry.
func (r *TrafficMonitorReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, r.Engine.Recompute(ctx)
}

// ClusterTrafficPolicyReconciler is the cluster-scoped sibling.
type ClusterTrafficPolicyReconciler struct {
	Engine *Engine
}

// Reconcile satisfies reconcile.Reconciler.
func (r *ClusterTrafficPolicyReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, r.Engine.Recompute(ctx)
}

// PodReconciler triggers a recompute on Pod changes (scheduling,
// label updates, deletion). Phase 2 simplification: any Pod event
// triggers a full recompute. Phase 5+ would scope to "did this Pod's
// coverage change?" before re-dispatching.
type PodReconciler struct {
	Engine *Engine
}

// Reconcile satisfies reconcile.Reconciler.
func (r *PodReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, r.Engine.Recompute(ctx)
}
