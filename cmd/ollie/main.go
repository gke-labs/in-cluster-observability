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

// Command ollie is the default binary that ships with the project.
//
// v0.2 wired the OBI-sibling-container capture pipeline: the agent
// starts an OTLP receiver on loopback (consuming from the sibling
// OBI container) and an OBI config writer (under a shared volume).
//
// v0.3 (per ADR-0021) keeps the agent thin: OBI does K8s metadata
// enrichment via its informer, and the agent is the OBI config writer
// + OTLP receiver carve-out hook point for v0.4 (controller-driven
// filtering) and v0.5 (in-cluster store + query). Until the v0.4
// controller exists, --obi-instrument-ports seeds OBI's discovery so
// Application mode has something to attach to.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/storage/remote"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gke-labs/in-cluster-observability/internal/debugendpoint"
	"github.com/gke-labs/in-cluster-observability/internal/export"
	"github.com/gke-labs/in-cluster-observability/internal/scrapeauth"
	"github.com/gke-labs/in-cluster-observability/internal/store"
	"github.com/gke-labs/in-cluster-observability/internal/streamsvc"
	"github.com/gke-labs/in-cluster-observability/pkg/capture"
	streamv1 "github.com/gke-labs/in-cluster-observability/pkg/stream/pb/stream/v1"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "v0.4.0-dev"

func main() {
	versionOnly := flag.Bool("version", false, "print version and exit")

	// v0.2 capture flags.
	otlpGRPC := flag.String("otlp-grpc-addr", "127.0.0.1:4317", "loopback bind address for OTLP gRPC receiver (consumed from sibling OBI container)")
	otlpHTTP := flag.String("otlp-http-addr", "127.0.0.1:4318", "loopback bind address for OTLP HTTP receiver")
	obiConfig := flag.String("obi-config", "/etc/ollie/obi-config/config.yaml", "shared-volume path where the agent writes OBI's config (empty disables writing)")
	obiInstrumentPorts := flag.String("obi-instrument-ports", "", "seed OBI's discovery.instrument with one entry matching processes on these listening ports (OBI format: \"80\", \"80,8080\", \"8000-8999\"). v0.3 L7 smoke-test knob; harmless once the v0.4 controller pushes per-PID MonitoringSpecs.")
	obiExportEndpoint := flag.String("obi-export-endpoint", "", "override the OTLP endpoint the agent writes into OBI's config (otel_{metrics,traces}_export). Empty derives the loopback receiver (http://<otlp-http-addr>) — the production shape. Non-loopback values bypass this agent entirely; used by the contract-fixture recorder (tests/contract/obi, #151) and for debugging OBI's raw stream.")
	scrapeAddr := flag.String("scrape-addr", "0.0.0.0:9090", "bind address for the production Prometheus scrape endpoint at /metrics (empty disables). Per ADR-0021 this is the single scrape URL — exposes both agent self-obs and re-emitted OBI metrics.")
	scrapeAuth := flag.String("scrape-auth", "auto", "authn/authz for the scrape endpoint (#145): 'token' requires a bearer token, validated via TokenReview + SubjectAccessReview for `get` on nonResourceURL /metrics (grant via the ollie-metrics-reader ClusterRole; fail-closed); 'none' disables auth; 'auto' picks token when running in-cluster, none otherwise. Loopback requests are always exempt (pod-internal debugging).")
	scrapeAuthAudiences := flag.String("scrape-auth-audiences", "", "comma-separated token audiences required by --scrape-auth=token (projected-token binding). Empty accepts standard API-server-audience tokens — required for managed collectors (GMP), which can't mint custom audiences.")
	controllerAddr := flag.String("controller-addr", "", "gRPC target for the v0.4 ollie-controller (e.g. ollie-controller.ollie-system.svc:9102). Empty disables the controller client; agent runs in standalone v0.3 mode (--obi-instrument-ports seed).")
	storeDir := flag.String("store-dir", "/var/lib/ollie/tsdb", "data directory for the node-local metric store (tsdb blocks + WAL, per ADR-0025; the DaemonSet mounts an emptyDir here). Empty disables the store.")
	queryAddr := flag.String("query-addr", "0.0.0.0:9091", "bind address for the Prometheus remote-read endpoint at /api/v1/read, serving the node-local store to the query server's fan-out (#95). Empty (or a disabled store) disables it. Auth follows --scrape-auth: token mode requires `post` on nonResourceURL /api/v1/read (granted to the query server's SA by the ollie-query-reader ClusterRole).")
	spanRingCapacity := flag.Int("span-ring-capacity", 65536, "capacity of the in-memory span ring fed by OBI's raw trace stream (#84, ADR-0026 §5). 0 disables the ring.")
	streamAddr := flag.String("stream-addr", "0.0.0.0:9092", "bind address for the gRPC StreamService (#99): node-local CEL-filtered span subscriptions consumed by the query server's cluster mux. Empty (or a disabled span ring) disables. Auth follows --scrape-auth: token mode requires `post` on the nonResourceURL method path (ollie-stream-reader ClusterRole); loopback exempt.")
	exportOTLPEndpoint := flag.String("export-otlp-endpoint", "", "OTLP endpoint to relay captured telemetry to (#97/#98, ADR-0026 §6): host:port for grpc, base URL for http. The ORIGINAL payloads received from OBI are forwarded — no re-encoding. Empty disables. Delivery is at-most-once: bounded queue, drop-on-full, 3 attempts with backoff (ollie_export_* metrics account for every drop).")
	exportOTLPProtocol := flag.String("export-otlp-protocol", "grpc", "OTLP relay protocol: grpc or http")
	exportOTLPHeaders := flag.String("export-otlp-headers", "", "comma-separated key=value headers attached to every OTLP export (auth tokens etc.)")
	exportOTLPCompression := flag.String("export-otlp-compression", "", "OTLP relay compression: gzip or empty")
	exportOTLPTimeout := flag.Duration("export-otlp-timeout", 10*time.Second, "per-attempt OTLP delivery timeout")
	exportRWURL := flag.String("export-remote-write-url", "", "Prometheus remote-write v1 endpoint to push the agent's metric state to (ADR-0026 §6). Snapshots the same gathered-sample stream the local store ingests, every --export-remote-write-interval. Empty disables.")
	exportRWHeaders := flag.String("export-remote-write-headers", "", "comma-separated key=value headers for remote write")
	exportRWInterval := flag.Duration("export-remote-write-interval", 15*time.Second, "remote-write snapshot cadence")
	nodeName := flag.String("node-name", os.Getenv("KUBE_NODE_NAME"), "K8s node this agent runs on. Defaults to $KUBE_NODE_NAME (populated via Downward API in k8s/daemonset.yaml).")
	debugEnable := flag.Bool("debug-endpoint", false, "enable the loopback debug HTTP endpoint on 127.0.0.1:9099 (off by default per ADR-0017.3)")
	debugAddr := flag.String("debug-endpoint-addr", debugendpoint.DefaultAddr, "loopback bind address for the debug endpoint")

	// v0.1 compatibility: stay-alive made sense when there was no real
	// work to do. v0.2's agent always blocks on signals; the flag is
	// kept for backward compat but is a no-op now.
	_ = flag.Bool("stay-alive", false, "deprecated in v0.2; agent always blocks on signals")
	flag.Parse()

	if *versionOnly {
		fmt.Println(version)
		return
	}

	fmt.Fprintf(os.Stderr, "ollie %s\n", version)
	fmt.Fprintln(os.Stderr, "v0.3: OTLP receiver + OBI config writer; OBI does K8s enrichment (per ADR-0021)")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// One MeterProvider feeds everything: agent self-obs counters
	// (ollie_capture_*) and the metric forwarder that re-emits OBI's
	// translated metrics. The same Prometheus handler backs the
	// production scrape listener on :9090 and the optional
	// /debug/metrics on the loopback debug endpoint.
	mp, promReg, promHandler, err := capture.NewPromMeterProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "prometheus exporter init failed: %v\n", err)
		os.Exit(1)
	}

	// Always-on "agent is alive and scrape path is wired" signal.
	// The OTel SDK only materializes a metric stream on first Add/
	// Record, so a freshly-started agent with no traffic produces an
	// empty /metrics aside from target_info. This gauge gives
	// scrapers a non-empty signal from boot.
	if up, err := mp.Meter("ollie/agent").Float64Gauge("ollie_agent_up",
		metric.WithDescription("1 if the ollie agent is running and its scrape path is wired"),
	); err == nil {
		up.Record(ctx, 1, metric.WithAttributes(
			attribute.String("version", version),
		))
	}

	// Raw-tee consumers (ADR-0026 §5–6): the span ring (#84) and the
	// OTLP export relays (#97/#98) both take OBI's payloads in wire
	// shape, straight off the bridge.
	tee := &agentRawTee{}
	var spanBuf *store.SpanBuffer
	if *spanRingCapacity > 0 {
		spanBuf = store.NewSpanBuffer(*spanRingCapacity, promReg)
		tee.spans = spanBuf
		fmt.Fprintf(os.Stderr, "span ring: capacity %d\n", *spanRingCapacity)
	}
	// Node-local stream service (#99): the query server subscribes
	// here and multiplexes across agents.
	if spanBuf != nil && *streamAddr != "" {
		var opts []grpc.ServerOption
		mw, err := buildStreamAuth(*scrapeAuth, *scrapeAuthAudiences)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream auth init failed: %v\n", err)
			os.Exit(1)
		}
		if mw != nil {
			opts = append(opts, grpc.StreamInterceptor(mw.StreamInterceptor()))
		}
		gs := grpc.NewServer(opts...)
		streamv1.RegisterStreamServiceServer(gs, &streamsvc.AgentServer{Spans: spanBuf})
		sl, err := net.Listen("tcp", *streamAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream listen %s: %v\n", *streamAddr, err)
			os.Exit(1)
		}
		go func() {
			if err := gs.Serve(sl); err != nil {
				fmt.Fprintf(os.Stderr, "stream server: %v\n", err)
			}
		}()
		defer gs.GracefulStop()
		fmt.Fprintf(os.Stderr, "stream service: %s\n", sl.Addr())
	}

	var exportMetrics *export.Metrics
	if *exportOTLPEndpoint != "" || *exportRWURL != "" {
		exportMetrics = export.NewMetrics(promReg)
	}
	if *exportOTLPEndpoint != "" {
		relays, err := export.NewOTLPRelays(export.OTLPConfig{
			Endpoint:    *exportOTLPEndpoint,
			Protocol:    *exportOTLPProtocol,
			Headers:     parseHeaders(*exportOTLPHeaders),
			Compression: *exportOTLPCompression,
			Timeout:     *exportOTLPTimeout,
			Metrics:     exportMetrics,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "otlp export init failed: %v\n", err)
			os.Exit(1)
		}
		tee.otlp = relays
		go relays.Run(ctx)
		fmt.Fprintf(os.Stderr, "otlp export: %s (%s)\n", *exportOTLPEndpoint, *exportOTLPProtocol)
	}
	if *exportRWURL != "" {
		rw := export.NewRemoteWriter(export.RemoteWriteConfig{
			URL:      *exportRWURL,
			Headers:  parseHeaders(*exportRWHeaders),
			Interval: *exportRWInterval,
			Gatherer: promReg,
			Metrics:  exportMetrics,
		})
		go rw.Run(ctx)
		fmt.Fprintf(os.Stderr, "remote-write export: %s every %s\n", *exportRWURL, *exportRWInterval)
	}
	var rawTee capture.RawTee
	if tee.spans != nil || tee.otlp != nil {
		rawTee = tee
	}

	captureCfg := capture.Config{
		OTLPGRPCAddr:     *otlpGRPC,
		OTLPHTTPAddr:     *otlpHTTP,
		OBIEndpoint:      *obiExportEndpoint,
		ObiConfigPath:    *obiConfig,
		InitialOpenPorts: *obiInstrumentPorts,
		MeterProvider:    mp,
		RawTee:           rawTee,
	}

	mgr, err := capture.NewBridge(captureCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "capture init failed: %v\n", err)
		os.Exit(1)
	}
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "capture start failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		stopCtx, stopCancel := context.WithCancel(context.Background())
		defer stopCancel()
		_ = mgr.Stop(stopCtx)
	}()
	fmt.Fprintf(os.Stderr, "OTLP receiver: gRPC=%s HTTP=%s; OBI config: %s\n", *otlpGRPC, *otlpHTTP, *obiConfig)
	if *obiInstrumentPorts != "" {
		fmt.Fprintf(os.Stderr, "OBI smoke-test discovery seeded: open_ports=%s\n", *obiInstrumentPorts)
	}

	// Production Prometheus scrape listener.
	var scrapeServer *http.Server
	if *scrapeAddr != "" {
		scrapeHandler, err := buildScrapeHandler(promHandler, *scrapeAuth, *scrapeAuthAudiences)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scrape auth init failed: %v\n", err)
			os.Exit(1)
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", scrapeHandler)
		l, err := net.Listen("tcp", *scrapeAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scrape listen %s: %v\n", *scrapeAddr, err)
			os.Exit(1)
		}
		scrapeServer = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := scrapeServer.Serve(l); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "scrape server: %v\n", err)
			}
		}()
		defer func() {
			sCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer sCancel()
			_ = scrapeServer.Shutdown(sCtx)
		}()
		fmt.Fprintf(os.Stderr, "scrape endpoint: http://%s/metrics\n", l.Addr())
	}

	// Loopback debug endpoint (off by default).
	if *debugEnable {
		opts := []debugendpoint.Option{
			debugendpoint.WithExtraHandler("GET /debug/metrics", promHandler),
		}
		dbg, err := debugendpoint.New(mgr, *debugAddr, opts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "debug endpoint init failed: %v\n", err)
			os.Exit(1)
		}
		actualAddr, err := dbg.Start(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "debug endpoint start failed: %v\n", err)
			os.Exit(1)
		}
		defer dbg.Stop(context.Background())
		fmt.Fprintf(os.Stderr, "debug endpoint enabled on %s (loopback); /debug/metrics serves agent self-obs\n", actualAddr)
	}

	// Node-local metric store (v0.5, #78 / ADR-0025): a tsdb fed by a
	// 1s self-scrape of promReg, so PromQL over the store sees exactly
	// what the :9090 scrape endpoint serves. The query server fans
	// reads out across the per-node stores (phase 2).
	if *storeDir != "" {
		st, err := store.New(store.Config{Dir: *storeDir, MetricsRegisterer: promReg})
		if err != nil {
			fmt.Fprintf(os.Stderr, "store init failed: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if err := st.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "store close: %v\n", err)
			}
		}()
		ing := store.NewIngester(st, promReg, promReg, time.Second, nil)
		go ing.Run(ctx)
		fmt.Fprintf(os.Stderr, "metric store: %s (2m blocks, 10m retention, 1s ingest)\n", *storeDir)

		// Remote-read endpoint (#95, ADR-0025 §3): the query server
		// fans PromQL reads out to this per-node endpoint and merges
		// the raw series. Same auth posture as the scrape endpoint.
		if *queryAddr != "" {
			rh := remote.NewReadHandler(slog.Default(), nil, st.ReadQueryable(),
				func() config.Config { return config.Config{} },
				5e7 /* sample limit, matches Prometheus's default */, 10, 1<<20)
			readHandler, err := buildScrapeHandler(rh, *scrapeAuth, *scrapeAuthAudiences)
			if err != nil {
				fmt.Fprintf(os.Stderr, "read auth init failed: %v\n", err)
				os.Exit(1)
			}
			readMux := http.NewServeMux()
			readMux.Handle("/api/v1/read", readHandler)
			rl, err := net.Listen("tcp", *queryAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "read listen %s: %v\n", *queryAddr, err)
				os.Exit(1)
			}
			readServer := &http.Server{Handler: readMux, ReadHeaderTimeout: 5 * time.Second}
			go func() {
				if err := readServer.Serve(rl); err != nil && err != http.ErrServerClosed {
					fmt.Fprintf(os.Stderr, "read server: %v\n", err)
				}
			}()
			defer func() {
				rCtx, rCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer rCancel()
				_ = readServer.Shutdown(rCtx)
			}()
			fmt.Fprintf(os.Stderr, "remote-read endpoint: http://%s/api/v1/read\n", rl.Addr())
		}
	} else {
		fmt.Fprintln(os.Stderr, "metric store: disabled (--store-dir empty)")
	}

	// Forwarder + writer: re-emit each MetricEvent into the OTel SDK
	// Meter so OBI's translated metrics flow out via the same
	// Prometheus exporter the scrape listener serves. SpanEvent /
	// EdgeEvent are drained for now (v0.5 wires them into the store).
	fwd := newOBICollector(mp.Meter("ollie/obi-forwarder"))
	promReg.MustRegister(fwd)
	go func() {
		for ev := range mgr.Events() {
			if ev.Kind == capture.EventMetric && ev.Metric != nil {
				fwd.Record(ctx, *ev.Metric)
			}
		}
	}()

	// v0.4 controller client. Opt-in via --controller-addr. When
	// set, the agent connects to the controller's gRPC AgentSession
	// stream and applies received MonitoringSpec deltas via
	// capture.Manager.AllowPID / BlockPID. When unset, the agent
	// runs in standalone v0.3 mode (--obi-instrument-ports seed).
	if *controllerAddr != "" {
		if *nodeName == "" {
			fmt.Fprintln(os.Stderr, "controller client requires --node-name (or $KUBE_NODE_NAME); aborting")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "controller client: connecting to %s as node=%s\n", *controllerAddr, *nodeName)
		go runControllerClient(ctx, *controllerAddr, *nodeName, mgr, func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		})
	} else {
		fmt.Fprintln(os.Stderr, "controller client: disabled (--controller-addr empty); standalone v0.3 mode")
	}

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "received shutdown signal; draining")
}

// buildStreamAuth resolves the --scrape-auth mode into a scrapeauth
// middleware for the gRPC stream service, or nil for mode none.
func buildStreamAuth(mode, audiences string) (*scrapeauth.Middleware, error) {
	if mode == "auto" {
		if _, err := rest.InClusterConfig(); err == nil {
			mode = "token"
		} else {
			mode = "none"
		}
	}
	switch mode {
	case "none":
		fmt.Fprintln(os.Stderr, "stream auth: DISABLED")
		return nil, nil
	case "token":
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("stream auth requires in-cluster credentials: %w", err)
		}
		client, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		var auds []string
		if audiences != "" {
			auds = strings.Split(audiences, ",")
		}
		return scrapeauth.New(scrapeauth.Config{Client: client, Audiences: auds, ExemptLoopback: true}), nil
	default:
		return nil, fmt.Errorf("unknown auth mode %q", mode)
	}
}

// buildScrapeHandler applies the --scrape-auth mode to the Prometheus
// handler. Mode "auto" resolves to "token" when an in-cluster config
// is available (the production DaemonSet), "none" otherwise (dev
// boxes, `go run`, Kind exec-based smoke flows outside a pod).
//
// The loopback debug endpoint (--debug-endpoint) intentionally stays
// unauthenticated: it binds 127.0.0.1 only, the same trust boundary
// as the loopback exemption here.
func buildScrapeHandler(promHandler http.Handler, mode, audiences string) (http.Handler, error) {
	if mode == "auto" {
		if _, err := rest.InClusterConfig(); err == nil {
			mode = "token"
		} else {
			mode = "none"
		}
		fmt.Fprintf(os.Stderr, "scrape auth: auto resolved to %s\n", mode)
	}
	switch mode {
	case "none":
		fmt.Fprintln(os.Stderr, "scrape auth: DISABLED — /metrics is unauthenticated on the scrape address")
		return promHandler, nil
	case "token":
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("--scrape-auth=token requires in-cluster credentials: %w", err)
		}
		client, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("building clientset: %w", err)
		}
		var auds []string
		if audiences != "" {
			auds = strings.Split(audiences, ",")
		}
		mw := scrapeauth.New(scrapeauth.Config{
			Client:         client,
			Audiences:      auds,
			ExemptLoopback: true,
		})
		fmt.Fprintf(os.Stderr, "scrape auth: token (TokenReview + SubjectAccessReview, audiences=%q)\n", audiences)
		return mw.Wrap(promHandler), nil
	default:
		return nil, fmt.Errorf("unknown --scrape-auth mode %q (want auto|token|none)", mode)
	}
}
