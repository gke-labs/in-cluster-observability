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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gke-labs/in-cluster-observability/internal/fanout"
	"github.com/gke-labs/in-cluster-observability/internal/queryapi"
	"github.com/gke-labs/in-cluster-observability/internal/scrapeauth"
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
