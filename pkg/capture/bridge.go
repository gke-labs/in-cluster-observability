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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	colllogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/gke-labs/in-cluster-observability/internal/obiconfig"
	"github.com/gke-labs/in-cluster-observability/internal/otlpreceiver"
)

// Small helpers for the standard counter attribute sets.
func metricAttrResult(result string) metric.AddOption {
	return metric.WithAttributes(attribute.String("result", result))
}

func metricAttrModule(m Module) metric.AddOption {
	return metric.WithAttributes(attribute.String("module", m.String()))
}

func metricAttrKind(k EventKind) metric.AddOption {
	return metric.WithAttributes(attribute.String("kind", eventKindString(k)))
}

func metricAttrReason(reason string) metric.AddOption {
	return metric.WithAttributes(attribute.String("reason", reason))
}

func eventKindString(k EventKind) string {
	switch k {
	case EventMetric:
		return "metric"
	case EventSpan:
		return "span"
	case EventEdge:
		return "edge"
	case EventModuleDegraded:
		return "module_degraded"
	default:
		return "unknown"
	}
}

// NewBridge constructs the sibling-container Manager: an OTLP receiver
// on loopback + an OBI config writer. The sibling OBI container is
// expected to push to the OTLP endpoints and watch the config file
// (per ADR-0018). NewBridge does not bind ports or write files; that
// happens in Start.
//
// Stability: Experimental
func NewBridge(cfg Config) (Manager, error) {
	cfg.applyDefaults()

	m, err := NewMetrics(cfg.MeterProvider)
	if err != nil {
		return nil, fmt.Errorf("capture: metrics init: %w", err)
	}

	b := &bridgeManager{
		cfg:            cfg,
		events:         make(chan Event, cfg.EventBuffer),
		modules:        map[Module]struct{}{},
		pids:           map[uint32]PIDSpec{},
		pods:           map[string]PodSpec{},
		metrics:        m,
		dirty:          make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		coalDone:       make(chan struct{}),
		debounceWindow: 500 * time.Millisecond,
	}

	if cfg.ObiConfigPath != "" {
		w, err := obiconfig.NewWriter(cfg.ObiConfigPath)
		if err != nil {
			return nil, fmt.Errorf("capture: obi config writer: %w", err)
		}
		b.writer = w
	}

	return b, nil
}

// bridgeManager implements Manager via OTLP receivers (loopback) and
// an OBI config writer. The agent does not invoke OBI as a library;
// OBI runs as a sibling container per ADR-0018.
type bridgeManager struct {
	cfg     Config
	metrics *Metrics

	mu        sync.Mutex
	started   bool
	stopped   bool
	modules   map[Module]struct{}
	pids      map[uint32]PIDSpec
	pods      map[string]PodSpec
	enrichers []Enricher

	writer   *obiconfig.Writer
	receiver *otlpreceiver.Server
	events   chan Event

	// reload coalescer infrastructure (per obi-integration.md §5):
	// non-blocking triggerReload posts to dirty; coalescerLoop consumes
	// dirty events with a debounce window before writing the OBI config.
	dirty    chan struct{}
	stopCh   chan struct{}
	coalDone chan struct{}

	// debounceWindow is configurable for tests; defaults to 500ms.
	debounceWindow time.Duration
}

// Start binds the OTLP receivers (if addresses are configured) and
// writes the initial OBI config (if a path is configured). Returns an
// error if any step fails; partial-startup teardown is best-effort.
func (b *bridgeManager) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return ErrStopped
	}
	if b.started {
		b.mu.Unlock()
		return nil
	}
	b.started = true
	b.mu.Unlock()

	// OTLP receiver — only bind if at least one address is configured.
	if b.cfg.OTLPGRPCAddr != "" || b.cfg.OTLPHTTPAddr != "" {
		recv, err := otlpreceiver.New(otlpreceiver.Config{
			GRPCAddr: b.cfg.OTLPGRPCAddr,
			HTTPAddr: b.cfg.OTLPHTTPAddr,
			Handler:  &bridgeHandler{b: b},
		})
		if err != nil {
			return fmt.Errorf("capture: receiver new: %w", err)
		}
		if err := recv.Start(ctx); err != nil {
			return fmt.Errorf("capture: receiver start: %w", err)
		}
		b.receiver = recv
	}

	// Initial OBI config — empty discovery list; modules are off until
	// EnableModule is called.
	if b.writer != nil {
		if _, err := b.writer.Write(b.buildConfig()); err != nil {
			return fmt.Errorf("capture: initial obi config: %w", err)
		}
	}

	// Reload coalescer — only run if a writer is configured.
	if b.writer != nil {
		go b.coalescerLoop()
	} else {
		close(b.coalDone)
	}
	return nil
}

// Stop drains the receiver and closes the Events channel. Idempotent.
func (b *bridgeManager) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	recv := b.receiver
	started := b.started
	b.mu.Unlock()

	close(b.stopCh)
	if started && b.writer != nil {
		<-b.coalDone
	}
	if recv != nil {
		_ = recv.Stop(ctx)
	}
	close(b.events)
	return nil
}

// AllowPID adds (or updates) a per-PID monitoring spec. The discovery
// section of OBI's config is derived from the current pid set; a
// reload signal is sent (debounced by the coalescer).
func (b *bridgeManager) AllowPID(pid uint32, spec PIDSpec) error {
	b.mu.Lock()
	if _, existed := b.pids[pid]; !existed {
		b.metrics.ActivePIDs.Add(context.Background(), 1)
	}
	b.pids[pid] = spec
	b.mu.Unlock()
	b.triggerReload()
	return nil
}

// BlockPID removes a per-PID spec. Idempotent.
func (b *bridgeManager) BlockPID(pid uint32) error {
	b.mu.Lock()
	if _, existed := b.pids[pid]; existed {
		b.metrics.ActivePIDs.Add(context.Background(), -1)
		delete(b.pids, pid)
		b.mu.Unlock()
		b.triggerReload()
		return nil
	}
	b.mu.Unlock()
	return nil
}

// AllowPod adds (or updates) a per-pod monitoring spec. Each AllowPod
// entry produces one obiconfig.Instrument with k8s_pod_name +
// k8s_namespace matchers — OBI's own informer attaches those
// attributes to candidate processes and the match runs against them.
// No PID resolution on the agent side.
func (b *bridgeManager) AllowPod(uid string, spec PodSpec) error {
	if uid == "" {
		return fmt.Errorf("capture: AllowPod requires a non-empty pod UID")
	}
	b.mu.Lock()
	b.pods[uid] = spec
	b.mu.Unlock()
	b.triggerReload()
	return nil
}

// BlockPod removes a per-pod spec. Idempotent.
func (b *bridgeManager) BlockPod(uid string) error {
	b.mu.Lock()
	if _, existed := b.pods[uid]; existed {
		delete(b.pods, uid)
		b.mu.Unlock()
		b.triggerReload()
		return nil
	}
	b.mu.Unlock()
	return nil
}

// EnableModule adds the module to the active set and triggers a reload.
// Idempotent.
func (b *bridgeManager) EnableModule(m Module, _ ModuleConfig) error {
	b.mu.Lock()
	b.modules[m] = struct{}{}
	b.mu.Unlock()
	b.triggerReload()
	return nil
}

// DisableModule removes the module and triggers a reload.
func (b *bridgeManager) DisableModule(m Module) error {
	b.mu.Lock()
	delete(b.modules, m)
	b.mu.Unlock()
	b.triggerReload()
	return nil
}

// EnabledModules returns the current module set.
func (b *bridgeManager) EnabledModules() []Module {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Module, 0, len(b.modules))
	for m := range b.modules {
		out = append(out, m)
	}
	return out
}

// Events returns the channel of translated capture events. Closed on Stop.
func (b *bridgeManager) Events() <-chan Event { return b.events }

// AddEnricher appends an enricher to the hot-path hook list.
func (b *bridgeManager) AddEnricher(e Enricher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enrichers = append(b.enrichers, e)
}

// Metrics returns the self-observability handle.
func (b *bridgeManager) Metrics() *Metrics { return b.metrics }

// triggerReload posts a non-blocking dirty signal to the coalescer.
// If the coalescer is already pending, the signal is dropped (it's
// already going to reload).
func (b *bridgeManager) triggerReload() {
	select {
	case b.dirty <- struct{}{}:
	default:
	}
}

// coalescerLoop debounces rapid AllowPID/BlockPID/EnableModule calls
// over debounceWindow before writing the OBI config, so a workload
// rollout doesn't thrash OBI's reload. Per obi-integration.md §5.
func (b *bridgeManager) coalescerLoop() {
	defer close(b.coalDone)
	for {
		select {
		case <-b.stopCh:
			return
		case <-b.dirty:
			// Wait for a quiet period.
			timer := time.NewTimer(b.debounceWindow)
		debounce:
			for {
				select {
				case <-b.stopCh:
					timer.Stop()
					return
				case <-b.dirty:
					// More activity — reset the timer.
					if !timer.Stop() {
						<-timer.C
					}
					timer.Reset(b.debounceWindow)
				case <-timer.C:
					break debounce
				}
			}
			b.writeReload()
		}
	}
}

// writeReload computes the desired OBI config from the current
// module/pid state and writes it atomically. Result is reported via
// ObiReloadsTotal{result=success|failure|noop}.
func (b *bridgeManager) writeReload() {
	file := b.buildConfig()
	changed, err := b.writer.Write(file)
	ctx := context.Background()
	switch {
	case err != nil:
		b.metrics.ObiReloadsTotal.Add(ctx, 1, metricAttrResult("failure"))
	case !changed:
		// No-op: identical content, no actual reload happens. Don't
		// tick the counter — operators expect this counter to reflect
		// actual OBI reloads.
	default:
		b.metrics.ObiReloadsTotal.Add(ctx, 1, metricAttrResult("success"))
	}
}

// buildConfig derives an OBI config from the current bridgeManager
// state. Each tracked PID becomes one Instrument with `target_pids`;
// each tracked pod (the v0.4 controller-driven path) becomes one
// Instrument with `k8s_pod_name` + `k8s_namespace` matchers. The
// smoke-port seed is added only when both sets are empty so the
// agent has something for OBI to attach to before any controller
// or operator drives discovery.
func (b *bridgeManager) buildConfig() obiconfig.File {
	b.mu.Lock()
	defer b.mu.Unlock()

	file := obiconfig.DefaultFile(b.cfg.OBIEndpoint)
	if len(b.pids) == 0 && len(b.pods) == 0 {
		if b.cfg.InitialOpenPorts != "" {
			file.Discovery.Instrument = []obiconfig.Instrument{{
				Name:      "smoke",
				OpenPorts: b.cfg.InitialOpenPorts,
			}}
		}
		return file
	}
	entries := make([]obiconfig.Instrument, 0, len(b.pids)+len(b.pods))
	for pid, spec := range b.pids {
		entries = append(entries, obiconfig.Instrument{
			Name:       fmt.Sprintf("pid-%d", pid),
			TargetPIDs: []uint32{pid},
			OpenPorts:  portsFromSpec(spec),
		})
	}
	// Sort pod UIDs so the emitted YAML is deterministic across
	// reconciles; the writer short-circuits unchanged content via
	// byte equality.
	uids := make([]string, 0, len(b.pods))
	for uid := range b.pods {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		spec := b.pods[uid]
		entries = append(entries, obiconfig.Instrument{
			Name:         "pod-" + shortUID(uid),
			K8sPodName:   spec.PodName,
			K8sNamespace: spec.Namespace,
			OpenPorts:    portsFromPodSpec(spec),
		})
	}
	file.Discovery.Instrument = entries
	return file
}

// portsFromSpec extracts open_ports from a PIDSpec as OBI's
// comma-separated string format. v0.2/v0.3 have no port info in the
// spec; controllers populate this in v0.4 via PodSpec. Returns "" for
// now.
func portsFromSpec(_ PIDSpec) string { return "" }

// portsFromPodSpec joins PodSpec.HTTPPorts into OBI's IntEnum string
// form ("80" / "80,8080" / "8000-8999"). v0.4 emits the comma-list
// form; ranges arrive when CR cardinality controls land in v0.6.
// Empty PodSpec.HTTPPorts produces "" — the entry still matches
// pods by K8s metadata (L4 socket filter captures TCP regardless of
// per-pod L7 attach).
func portsFromPodSpec(spec PodSpec) string {
	if len(spec.HTTPPorts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(spec.HTTPPorts))
	for _, p := range spec.HTTPPorts {
		parts = append(parts, strconv.FormatUint(uint64(p), 10))
	}
	return strings.Join(parts, ",")
}

// shortUID returns the first 12 characters of a K8s UID — enough to
// be unique within a single agent's tracked pod set and short enough
// to keep the OBI Instrument name human-scannable in the rendered
// config file. Falls through to the raw UID if it's already short.
func shortUID(uid string) string {
	if len(uid) <= 12 {
		return uid
	}
	return uid[:12]
}

// bridgeHandler implements otlpreceiver.Handler. v0.2 forwards each
// payload to the translator (#72, #73). For #70 the handler just
// counts; per-protocol translation lands in subsequent commits.
type bridgeHandler struct {
	b *bridgeManager
}

func (h *bridgeHandler) OnMetrics(ctx context.Context, req *collmetricspb.ExportMetricsServiceRequest) error {
	defer h.b.recoverPanic("receiver_metrics", ModuleL4TCP)
	for _, ev := range TranslateMetrics(req.GetResourceMetrics()) {
		h.emit(ctx, ev)
	}
	return nil
}

func (h *bridgeHandler) OnTraces(ctx context.Context, req *colltracepb.ExportTraceServiceRequest) error {
	defer h.b.recoverPanic("receiver_traces", ModuleHTTP1)
	for _, ev := range TranslateTraces(req.GetResourceSpans()) {
		h.emit(ctx, ev)
	}
	return nil
}

func (h *bridgeHandler) OnLogs(ctx context.Context, _ *colllogspb.ExportLogsServiceRequest) error {
	defer h.b.recoverPanic("receiver_logs", ModuleL4TCP)
	// Logs not used in v0.2; drop silently.
	return nil
}

// emit pushes ev onto the Events() channel non-blockingly, ticking the
// EventsTotal counter on success and EventsDroppedTotal on backpressure.
// Dropped events do not block the OTLP receiver — the hot path stays cold.
func (h *bridgeHandler) emit(ctx context.Context, ev Event) {
	select {
	case h.b.events <- ev:
		h.b.metrics.EventsTotal.Add(ctx, 1,
			metricAttrModule(ev.Module),
			metricAttrKind(ev.Kind),
		)
	default:
		h.b.metrics.EventsDroppedTotal.Add(ctx, 1,
			metricAttrReason("backpressure"),
		)
	}
}
