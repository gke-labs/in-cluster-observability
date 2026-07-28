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

package capture

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// recoverPanic is a guard for hot-path goroutines. Run it via defer
// at the top of any function that runs in a goroutine which the agent
// process cannot afford to lose to a panic. On recovery it ticks
// PanicsTotal{component=name} and emits a Module-degraded event for
// the supplied module so downstream consumers can react.
func (b *bridgeManager) recoverPanic(component string, mod Module) {
	if r := recover(); r != nil {
		slog.Error("capture: recovered panic",
			"component", component,
			"module", mod.String(),
			"panic", r,
			"stack", string(debug.Stack()),
		)
		ctx := context.Background()
		b.metrics.PanicsTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("component", component)),
		)
		b.markDegraded(ctx, mod, fmt.Sprintf("panic: %v", r))
	}
}

// markDegraded emits a capture.Event{Kind:ModuleDegraded} for the
// supplied module. Non-blocking: if the events channel is full the
// degraded event is dropped (operators still see PanicsTotal /
// ObiRestartsTotal increments). Reason is a short human-readable
// string ("panic", "obi_restart_threshold", "config_rejected").
func (b *bridgeManager) markDegraded(ctx context.Context, mod Module, reason string) {
	ev := Event{
		Kind:      EventModuleDegraded,
		Timestamp: time.Now(),
		Module:    mod,
	}
	select {
	case b.events <- ev:
		b.metrics.EventsTotal.Add(ctx, 1,
			metricAttrModule(mod),
			metricAttrKind(EventModuleDegraded),
		)
	default:
		b.metrics.EventsDroppedTotal.Add(ctx, 1,
			metricAttrReason("backpressure_degraded"),
		)
	}
	_ = reason // reason is currently surfaced only via the structured log.
}

// ReportOBIRestart advances the obi_restarts_total counter by the
// delta between the supplied cumulative restart count and the highest
// count previously reported (so repeated reports of the same count
// don't inflate the metric — #154), and, if the count crosses
// degradedThreshold, emits a ModuleDegraded event covering every
// currently-enabled module. The agent does not poll for restarts
// itself in v0.2 — operators or a future controller (#77 v0.3
// follow-up) call this method when they detect OBI's container
// restart count has increased. No-op after Stop.
//
// Stability: Experimental
func (b *bridgeManager) ReportOBIRestart(ctx context.Context, restartCount int64) {
	if !b.beginEmit() {
		return // stopped; drop
	}
	defer b.endEmit()

	b.mu.Lock()
	delta := restartCount - b.lastRestartCount
	if delta > 0 {
		b.lastRestartCount = restartCount
	}
	mods := make([]Module, 0, len(b.modules))
	for m := range b.modules {
		mods = append(mods, m)
	}
	b.mu.Unlock()

	if delta > 0 {
		b.metrics.ObiRestartsTotal.Add(ctx, delta)
	}
	const degradedThreshold = 3
	if restartCount < degradedThreshold || len(mods) == 0 {
		return
	}
	for _, m := range mods {
		b.markDegraded(ctx, m, fmt.Sprintf("obi_restart_count=%d", restartCount))
	}
}
