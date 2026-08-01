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

// Command ollie-query is the v0.5 query server (#94, #95): a
// stateless Deployment that discovers the agent DaemonSet through its
// headless Service, fans PromQL reads out to every agent's
// remote-read endpoint, and serves the Prometheus-compatible HTTP API
// (/api/v1/query, /api/v1/query_range) plus health probes.
//
// All data lives on the agents (10-minute windows, per ADR-0012);
// this process holds no state and scales horizontally.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gke-labs/in-cluster-observability/internal/ca"
	"github.com/gke-labs/in-cluster-observability/internal/custommetrics"
	"github.com/gke-labs/in-cluster-observability/internal/fanout"
	"github.com/gke-labs/in-cluster-observability/internal/frontproxy"
	"github.com/gke-labs/in-cluster-observability/internal/queryapi"
	"github.com/gke-labs/in-cluster-observability/internal/scrapeauth"
	"github.com/gke-labs/in-cluster-observability/internal/streamsvc"
	streamv1 "github.com/gke-labs/in-cluster-observability/pkg/stream/pb/stream/v1"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "v0.5.0-dev"

// defaultTokenFile is the in-cluster ServiceAccount token the query
// server presents to the agents' authenticated read endpoints.
const defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // path, not a credential

func main() {
	versionOnly := flag.Bool("version", false, "print version and exit")
	httpAddr := flag.String("http-addr", "0.0.0.0:9095", "bind address for the query HTTP API and health probes")
	agentService := flag.String("agent-service", "ollie-agent.ollie-system.svc.cluster.local", "headless-Service DNS name resolving to one A record per agent pod (ADR-0025 §5)")
	agentPort := flag.Int("agent-port", 9091, "port of the agents' remote-read endpoint")
	agentAddrs := flag.String("agent-addrs", "", "comma-separated static agent host:port list, bypassing DNS discovery (dev/debug)")
	resolveInterval := flag.Duration("resolve-interval", 15*time.Second, "how often to re-resolve --agent-service")
	queryTimeout := flag.Duration("query-timeout", 30*time.Second, "overall per-query deadline; each agent gets 0.8x the remainder")
	tokenFile := flag.String("agent-token-file", defaultTokenFile, "bearer token file presented to the agents' authenticated read endpoints (empty disables)")
	apiAuth := flag.String("api-auth", "auto", "authn/authz for /api/v1/* (same posture as the agent's --scrape-auth): 'token' validates bearer tokens via TokenReview + SubjectAccessReview against the request path as nonResourceURL (grant via the ollie-promql-reader ClusterRole); 'none' disables; 'auto' picks token in-cluster. Health probes are always unauthenticated; loopback is always exempt (port-forward debugging).")
	apiAuthAudiences := flag.String("api-auth-audiences", "", "comma-separated token audiences for --api-auth=token; empty accepts standard API-server-audience tokens")
	tlsAddr := flag.String("tls-addr", "0.0.0.0:6443", "bind address for the HTTPS listener serving custom.metrics.k8s.io to the aggregation layer (#96). Empty disables.")
	tlsHostnames := flag.String("tls-hostnames", "ollie-query.ollie-system.svc,ollie-query.ollie-system.svc.cluster.local,localhost", "comma-separated DNS SANs for the self-signed bootstrap cert used until the CA-issued serving cert is mounted")
	tlsCertFile := flag.String("tls-cert-file", "/etc/ollie/tls/tls.crt", "PEM serving cert issued by the self-managed CA (ADR-0028), mounted from the ollie-query-serving Secret; hot-reloaded on rotation. Falls back to a self-signed bootstrap cert when absent (fresh install / dev).")
	tlsKeyFile := flag.String("tls-key-file", "/etc/ollie/tls/tls.key", "PEM serving key paired with --tls-cert-file")
	cmClientAuth := flag.String("custom-metrics-client-auth", "auto", "authn for the :6443 custom-metrics listener. 'requestheader' requires a client certificate signed by the cluster's front-proxy (requestheader) CA — the identity the aggregation layer presents — so no unauthenticated pod can read cluster-wide metrics; the CA is loaded from kube-system/extension-apiserver-authentication (grant via the ollie-query SA's binding to extension-apiserver-authentication-reader). 'none' disables client-cert auth (dev/`go run` only; the port is then unauthenticated). 'auto' picks requestheader in-cluster, none otherwise.")
	cmConfig := flag.String("custom-metrics-config", "/etc/ollie/custom-metrics/config.yaml", "path to the metric-path -> PromQL template config (ConfigMap-mounted; missing file falls back to built-in defaults)")
	streamAddr := flag.String("stream-addr", "0.0.0.0:9096", "bind address for the cluster-wide gRPC StreamService (#99): CEL span subscriptions multiplexed across agents + periodic PromQL streams. Empty disables. Auth follows --api-auth (ollie-stream-reader ClusterRole; loopback exempt).")
	agentStreamPort := flag.Int("agent-stream-port", 9092, "port of the agents' node-local StreamService")
	flag.Parse()

	if *versionOnly {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	logger.Info("ollie-query starting", "version", version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	disc := fanout.NewDiscoverer(*agentService, *agentPort, *resolveInterval, logger)
	if *agentAddrs != "" {
		disc.SetStaticAddrs(strings.Split(*agentAddrs, ","))
		logger.Info("agent discovery: static", "addrs", *agentAddrs)
	} else {
		go disc.Run(ctx)
	}

	// Tokens are only readable (and only useful) in-cluster; drop the
	// default silently when the file isn't there so `go run` works.
	tf := *tokenFile
	if tf != "" {
		if _, err := os.Stat(tf); err != nil {
			if tf != defaultTokenFile {
				logger.Error("agent token file unreadable", "path", tf, "err", err)
				os.Exit(1)
			}
			tf = ""
			logger.Info("agent auth: no in-cluster token; sending unauthenticated reads")
		}
	}

	queryable := fanout.NewQueryable(fanout.Config{
		Discoverer:      disc,
		BearerTokenFile: tf,
		Timeout:         *queryTimeout,
		Logger:          logger,
	})

	api := queryapi.New(queryapi.Config{
		Queryable: queryable,
		Ready:     disc.Ready,
		Timeout:   *queryTimeout,
		Logger:    logger,
	})

	handler, err := buildAPIHandler(api, *apiAuth, *apiAuthAudiences, logger)
	if err != nil {
		logger.Error("api auth init failed", "err", err)
		os.Exit(1)
	}

	// Cluster-wide stream service (#99): span subscriptions fan out
	// to the agents' node-local stream ports; metric streams are
	// periodic evaluations over the same fan-out queryable.
	if *streamAddr != "" {
		streamDisc := &portRewriter{disc: disc, port: *agentStreamPort}
		qs := &streamsvc.QueryServer{
			Agents: streamDisc,
			Eval:   api,
			BearerToken: func() string {
				if tf == "" {
					return ""
				}
				raw, err := os.ReadFile(tf)
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(raw))
			},
			Logger: logger,
		}
		var opts []grpc.ServerOption
		mw, err := buildStreamAuth(*apiAuth, *apiAuthAudiences, logger)
		if err != nil {
			logger.Error("stream auth init failed", "err", err)
			os.Exit(1)
		}
		if mw != nil {
			opts = append(opts, grpc.StreamInterceptor(mw.StreamInterceptor()))
		}
		gs := grpc.NewServer(opts...)
		streamv1.RegisterStreamServiceServer(gs, qs)
		sl, err := net.Listen("tcp", *streamAddr)
		if err != nil {
			logger.Error("stream listen failed", "addr", *streamAddr, "err", err)
			os.Exit(1)
		}
		go func() {
			if err := gs.Serve(sl); err != nil {
				logger.Error("stream server failed", "err", err)
			}
		}()
		defer gs.GracefulStop()
		logger.Info("stream service listening", "addr", sl.Addr().String())
	}

	// custom.metrics.k8s.io for the HPA (#96), on its own HTTPS
	// listener for the aggregation layer.
	if *tlsAddr != "" {
		cm, err := custommetrics.New(custommetrics.Config{
			Evaluator:  api,
			ConfigPath: *cmConfig,
			Logger:     logger,
		})
		if err != nil {
			logger.Error("custom-metrics init failed", "err", err)
			os.Exit(1)
		}
		// Serving cert: prefer the CA-issued cert mounted from the
		// ollie-query-serving Secret (ADR-0028), hot-reloaded on
		// rotation. Until the controller's CA manager has issued it
		// (fresh install) or when running outside a cluster (dev), fall
		// back to a self-signed bootstrap cert — the APIService stays
		// insecureSkipTLSVerify:true until the controller confirms every
		// endpoint serves the CA cert and flips it.
		bootstrap, err := custommetrics.SelfSignedCert(strings.Split(*tlsHostnames, ","))
		if err != nil {
			logger.Error("self-signed cert generation failed", "err", err)
			os.Exit(1)
		}
		reloader := ca.NewReloader(*tlsCertFile, *tlsKeyFile, logger)
		getCertificate := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if cert, rErr := reloader.GetCertificate(hello); rErr == nil {
				return cert, nil
			}
			return &bootstrap, nil
		}
		tlsConf := &tls.Config{GetCertificate: getCertificate, MinVersion: tls.VersionTLS12}
		cmHandler := http.Handler(cm.Routes())

		// Require the aggregation layer's front-proxy client certificate
		// (#96 auth hole): without it, any pod that can reach :6443 reads
		// cluster-wide metrics unauthenticated. The kube-apiserver
		// authenticates to aggregated backends with a requestheader-CA-
		// signed client cert, so pinning ClientCAs to that CA admits only
		// the aggregator at the TLS layer.
		fp, err := customMetricsClientAuth(ctx, *cmClientAuth, logger)
		if err != nil {
			logger.Error("custom-metrics client auth init failed", "err", err)
			os.Exit(1)
		}
		if fp != nil {
			tlsConf.ClientCAs = fp.ClientCAs()
			tlsConf.ClientAuth = tls.RequireAndVerifyClientCert
			cmHandler = fp.Middleware(cmHandler)
			logger.Info("custom-metrics client auth: requestheader (mTLS)", "allowed-names", fp.AllowedNames())
		}

		tlsSrv := &http.Server{
			Addr:              *tlsAddr,
			Handler:           cmHandler,
			ReadHeaderTimeout: 5 * time.Second,
			TLSConfig:         tlsConf,
		}
		go func() {
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				logger.Error("custom-metrics server failed", "err", err)
				cancel()
			}
		}()
		defer func() {
			sCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer sCancel()
			_ = tlsSrv.Shutdown(sCtx)
		}()
		logger.Info("custom-metrics API listening", "addr", *tlsAddr, "group-version", custommetrics.GroupVersion)
	}

	l, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		logger.Error("listen failed", "addr", *httpAddr, "err", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "err", err)
			cancel()
		}
	}()
	logger.Info("query API listening", "addr", l.Addr().String(), "agent-service", *agentService, "agent-port", *agentPort)

	<-ctx.Done()
	logger.Info("shutting down")
	sCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sCancel()
	_ = srv.Shutdown(sCtx)
}

// buildAPIHandler wraps the /api/v1/* routes in bearer-token auth
// (TokenReview + SubjectAccessReview, the agent's #145 posture) while
// leaving the kubelet-probed /healthz endpoints open. Mode "auto"
// resolves to "token" in-cluster, "none" otherwise.
func buildAPIHandler(api *queryapi.API, mode, audiences string, logger *slog.Logger) (http.Handler, error) {
	routes := api.Routes()
	if mode == "auto" {
		if _, err := rest.InClusterConfig(); err == nil {
			mode = "token"
		} else {
			mode = "none"
		}
		logger.Info("api auth: auto resolved", "mode", mode)
	}
	switch mode {
	case "none":
		logger.Warn("api auth: DISABLED — /api/v1/* is unauthenticated")
		return routes, nil
	case "token":
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("--api-auth=token requires in-cluster credentials: %w", err)
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
		mux := http.NewServeMux()
		mux.Handle("/api/", mw.Wrap(routes))
		mux.Handle("/healthz/", routes)
		logger.Info("api auth: token (TokenReview + SubjectAccessReview)", "audiences", audiences)
		return mux, nil
	default:
		return nil, fmt.Errorf("unknown --api-auth mode %q (want auto|token|none)", mode)
	}
}

// customMetricsClientAuth resolves the --custom-metrics-client-auth
// mode into a front-proxy authenticator for the :6443 listener, or nil
// when auth is disabled. Mode "auto" resolves to "requestheader"
// in-cluster (where the aggregator and the requestheader CA exist) and
// "none" otherwise.
func customMetricsClientAuth(ctx context.Context, mode string, logger *slog.Logger) (*frontproxy.Authenticator, error) {
	if mode == "auto" {
		if _, err := rest.InClusterConfig(); err == nil {
			mode = "requestheader"
		} else {
			mode = "none"
		}
		logger.Info("custom-metrics client auth: auto resolved", "mode", mode)
	}
	switch mode {
	case "none":
		logger.Warn("custom-metrics client auth: DISABLED — :6443 serves cluster-wide metrics unauthenticated (dev only)")
		return nil, nil
	case "requestheader":
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("--custom-metrics-client-auth=requestheader requires in-cluster credentials: %w", err)
		}
		client, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("building clientset: %w", err)
		}
		return frontproxy.Load(ctx, client)
	default:
		return nil, fmt.Errorf("unknown --custom-metrics-client-auth mode %q (want auto|requestheader|none)", mode)
	}
}

// portRewriter presents the fan-out discoverer's endpoints with the
// agents' stream port substituted for the remote-read port.
type portRewriter struct {
	disc *fanout.Discoverer
	port int
}

func (p *portRewriter) Addrs() []string {
	in := p.disc.Addrs()
	out := make([]string, 0, len(in))
	for _, a := range in {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		out = append(out, net.JoinHostPort(host, strconv.Itoa(p.port)))
	}
	return out
}

// buildStreamAuth mirrors buildAPIHandler's mode resolution for the
// gRPC stream listener; nil means auth disabled.
func buildStreamAuth(mode, audiences string, logger *slog.Logger) (*scrapeauth.Middleware, error) {
	if mode == "auto" {
		if _, err := rest.InClusterConfig(); err == nil {
			mode = "token"
		} else {
			mode = "none"
		}
	}
	switch mode {
	case "none":
		logger.Warn("stream auth: DISABLED")
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
