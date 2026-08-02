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

// Command ollie-controller is the v0.4 control-plane binary. It runs
// as a 2-replica Deployment in the install namespace, elects a leader
// via a Lease, watches TrafficMonitor + ClusterTrafficPolicy + Pod,
// computes per-pod MonitoringSpecs, and streams them to agents via the
// gRPC AgentSession bidirectional stream. Per ADR-0022, no validating
// webhook in v0.4 and no identity broadcasting.
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

	"google.golang.org/grpc"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/gke-labs/in-cluster-observability/internal/ca"
	v1alpha1 "github.com/gke-labs/in-cluster-observability/pkg/controller/api/v1alpha1"
	cppb "github.com/gke-labs/in-cluster-observability/pkg/controller/pb/controlplane/v1"
	"github.com/gke-labs/in-cluster-observability/pkg/controller/reconciler"
	"github.com/gke-labs/in-cluster-observability/pkg/controller/stream"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "v0.4.0-dev"

func main() {
	versionOnly := flag.Bool("version", false, "print version and exit")
	metricsAddr := flag.String("metrics-addr", ":9100", "bind address for the controller's /metrics endpoint (empty disables)")
	probeAddr := flag.String("probe-addr", ":9101", "bind address for /healthz + /readyz")
	streamAddr := flag.String("stream-addr", ":9102", "gRPC bind address for the agent AgentSession stream")
	leaderElection := flag.Bool("leader-elect", true, "enable Lease-based leader election (one replica accepts agent streams at a time)")
	leaderElectionID := flag.String("leader-election-id", "ollie-controller", "Lease name for leader election")
	leaderElectionNS := flag.String("leader-election-namespace", "", "namespace for the leader-election Lease; empty = in-cluster namespace")

	// Self-managed CA (ADR-0028, #196): the leader mints a CA, issues
	// the query serving cert, injects it into the custom-metrics
	// APIService caBundle, and drops insecureSkipTLSVerify once every
	// query endpoint serves it. No cert-manager dependency.
	enableCA := flag.Bool("enable-ca", true, "run the self-managed CA manager: mint the ollie CA, issue the query serving cert, and wire the custom-metrics APIService caBundle (ADR-0028)")
	caNamespace := flag.String("ca-namespace", "", "namespace holding the CA and serving-cert Secrets; empty = in-cluster namespace (falls back to ollie-system)")
	caSecret := flag.String("ca-secret", "ollie-ca", "Secret name for the self-managed CA cert+key")
	caServingSecret := flag.String("ca-serving-secret", "ollie-query-serving", "Secret name for the query serving cert+key issued by the CA")
	caAgentServingSecret := flag.String("ca-agent-serving-secret", "ollie-agent-serving", "Secret name for the agent serving cert+key issued by the CA (intra-ollie TLS, ADR-0029); empty disables")
	caAgentService := flag.String("ca-agent-service", "ollie-agent", "agent headless Service name; its DNS forms are the SANs of the agent serving cert")
	caQueryService := flag.String("ca-query-service", "ollie-query", "query Service whose ready endpoints back the custom-metrics :6443 port; gates the insecureSkipTLSVerify flip")
	caTLSPort := flag.Int("ca-tls-port", 6443, "query custom-metrics TLS port probed by the flip gate")
	caAPIService := flag.String("ca-apiservice", "v1beta1.custom.metrics.k8s.io", "APIService whose caBundle the CA manager populates")
	caServingLifetime := flag.Duration("ca-serving-lifetime", ca.ServingDefaultLifetime, "validity of issued query serving certs (renewed before expiry)")
	flag.Parse()

	if *versionOnly {
		fmt.Println(version)
		return
	}

	fmt.Fprintf(os.Stderr, "ollie-controller %s\n", version)
	fmt.Fprintln(os.Stderr, "v0.4 control plane: TrafficMonitor + ClusterTrafficPolicy → MonitoringSpec → agents (per ADR-0022)")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		fatalf("get kubeconfig: %v\n", err)
	}

	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        s,
		Metrics:                       metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress:        *probeAddr,
		LeaderElection:                *leaderElection,
		LeaderElectionID:              *leaderElectionID,
		LeaderElectionNamespace:       *leaderElectionNS,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		fatalf("NewManager: %v\n", err)
	}

	dispatcher := stream.NewDispatcher()
	agentState := stream.NewAgentStateStore()
	engine := &reconciler.Engine{
		Client:     mgr.GetClient(),
		Dispatcher: dispatcher,
		Agents:     agentState,
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("trafficmonitor").
		For(&v1alpha1.TrafficMonitor{}).
		Complete(&reconciler.TrafficMonitorReconciler{Engine: engine}); err != nil {
		fatalf("setup TrafficMonitorReconciler: %v\n", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("clustertrafficpolicy").
		For(&v1alpha1.ClusterTrafficPolicy{}).
		Complete(&reconciler.ClusterTrafficPolicyReconciler{Engine: engine}); err != nil {
		fatalf("setup ClusterTrafficPolicyReconciler: %v\n", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("pod-trigger").
		For(&corev1.Pod{}).
		Complete(&reconciler.PodReconciler{Engine: engine}); err != nil {
		fatalf("setup PodReconciler: %v\n", err)
	}

	// Self-managed CA manager (ADR-0028). Runs only on the elected
	// leader (NeedLeaderElection), so it is the single writer of the CA
	// Secret, the serving-cert Secret, and the APIService caBundle.
	if *enableCA {
		ns := *caNamespace
		if ns == "" {
			ns = inClusterNamespace()
		}
		clientset, cErr := kubernetes.NewForConfig(cfg)
		if cErr != nil {
			fatalf("build clientset for CA manager: %v\n", cErr)
		}
		dyn, dErr := dynamic.NewForConfig(cfg)
		if dErr != nil {
			fatalf("build dynamic client for CA manager: %v\n", dErr)
		}
		caMgr := &ca.Manager{
			Clientset:       clientset,
			APISvc:          ca.NewDynamicAPIServiceStore(dyn, *caAPIService),
			Namespace:       ns,
			CASecret:        *caSecret,
			ServingSecret:   *caServingSecret,
			ServingDNSNames: servingDNSNames(*caQueryService, ns),
			QueryService:    *caQueryService,
			TLSPort:         *caTLSPort,
			ServingLifetime: *caServingLifetime,

			AgentServingSecret:   *caAgentServingSecret,
			AgentServingDNSNames: servingDNSNames(*caAgentService, ns),
			Logger:               slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "ca-manager"),
		}
		if aErr := mgr.Add(caMgr); aErr != nil {
			fatalf("register CA manager: %v\n", aErr)
		}
		fmt.Fprintf(os.Stderr, "self-managed CA manager enabled (namespace=%s, apiservice=%s)\n", ns, *caAPIService)
	}

	if err := mgr.AddHealthzCheck("alive", func(_ *http.Request) error { return nil }); err != nil {
		fatalf("AddHealthzCheck: %v\n", err)
	}
	if err := mgr.AddReadyzCheck("ready", func(_ *http.Request) error { return nil }); err != nil {
		fatalf("AddReadyzCheck: %v\n", err)
	}

	// gRPC AgentSession server. Only the leader accepts streams;
	// followers reject with FailedPrecondition so agents reconnect
	// to the new leader on failover. mgr.Elected() closes when this
	// replica wins the lease.
	grpcServer := grpc.NewServer()
	cppb.RegisterControlPlaneServer(grpcServer, &stream.Server{
		Dispatcher: dispatcher,
		AgentState: agentState,
		IsLeader: func() bool {
			select {
			case <-mgr.Elected():
				return true
			default:
				return false
			}
		},
	})
	lis, err := net.Listen("tcp", *streamAddr)
	if err != nil {
		fatalf("stream listen %s: %v\n", *streamAddr, err)
	}
	go func() {
		fmt.Fprintf(os.Stderr, "gRPC stream server: listening on %s\n", lis.Addr())
		if err := grpcServer.Serve(lis); err != nil {
			fmt.Fprintf(os.Stderr, "gRPC stream server: %v\n", err)
		}
	}()
	defer grpcServer.GracefulStop()

	if err := mgr.Start(ctx); err != nil {
		fatalf("manager.Start: %v\n", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}

// inClusterNamespace returns the namespace this pod runs in, read from
// the ServiceAccount projection, defaulting to ollie-system when it is
// unreadable (e.g. `go run` outside a cluster).
func inClusterNamespace() string {
	const saNS = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	if b, err := os.ReadFile(saNS); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "ollie-system"
}

// servingDNSNames builds the SANs for the query serving cert from the
// Service name and namespace: the aggregator connects via the Service
// DNS, so both the short and cluster-local forms must be present.
func servingDNSNames(service, namespace string) []string {
	return []string{
		fmt.Sprintf("%s.%s.svc", service, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", service, namespace),
		"localhost",
	}
}
