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

// Command iobsctl is the ad-hoc query CLI (#100): PromQL against the
// query server's HTTP API and live CEL span subscriptions against
// its gRPC stream service, over an automatic port-forward.
//
// Per ADR-0026 §8 it consumes only the public wire surfaces — the
// same ones any third-party tool would. Port-forwarded requests
// arrive on the query server's loopback, which the auth layer
// exempts by design; in-cluster consumers bind ollie-promql-reader /
// ollie-stream-reader instead.
//
//	iobsctl metrics 'sum(rate(http_server_request_duration_count[1m]))'
//	iobsctl metrics 'ollie_agent_up' --start=-15m --step=30s
//	iobsctl spans --filter 'resource["k8s.namespace.name"] == "shop"'
//	iobsctl spans --max 10 --output json
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "v0.5.0-dev"

type globalOpts struct {
	kubeconfig string
	kubectx    string
	namespace  string
	service    string
	output     string
	timeout    time.Duration
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println(version)
	case "metrics":
		runMetrics(os.Args[2:])
	case "spans":
		runSpans(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `iobsctl — query in-cluster observability (ollie)

Subcommands:
  metrics <promql>   instant or range PromQL via the query server
      --time <rfc3339|unix>     instant evaluation time (default now)
      --start <rfc3339|-15m>    range start (turns the query into a range query)
      --end <rfc3339|unix>      range end (default now)
      --step <duration>         range step (default 15s)
  spans              live CEL-filtered span stream
      --filter <cel>            CEL over span (OTLP Span) + resource map
      --max <n>                 exit after n spans (default: stream forever)

Global flags (both subcommands):
  --kubeconfig, --context, --namespace (ollie-system), --service (ollie-query),
  --output text|json, --timeout (default 5m)
`)
}

func addGlobalFlags(fs *flag.FlagSet) *globalOpts {
	g := &globalOpts{}
	fs.StringVar(&g.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: standard loading rules)")
	fs.StringVar(&g.kubectx, "context", "", "kubeconfig context")
	fs.StringVar(&g.namespace, "namespace", "ollie-system", "namespace of the query server")
	fs.StringVar(&g.service, "service", "ollie-query", "query-server service/deployment name")
	fs.StringVar(&g.output, "output", "text", "output format: text|json")
	fs.DurationVar(&g.timeout, "timeout", 5*time.Minute, "overall command timeout")
	return g
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "iobsctl: "+format+"\n", args...)
	os.Exit(1)
}
