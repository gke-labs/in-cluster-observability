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

package obi

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gke-labs/in-cluster-observability/tests/e2e"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// recordFixtures gates TestRecordFixtures (issue #151). It stands up
// the full DaemonSet on Kind — exactly like tests/e2e — but repoints
// the obi container's OTLP exporter at a recorder running inside this
// test process on the host, so the captured request bodies are the
// real bytes a pinned OBI image puts on the wire. Run:
//
//	go test ./tests/contract/obi -record -timeout 20m
//	go test ./tests/contract/obi -update   # regenerate goldens
//
// Requires kind + docker + kubectl (same as `ap e2e .`). Not run in
// CI; fixtures are regenerated per OBI image bump (ADR-0010/0018).
var recordFixtures = flag.Bool("record", false, "record real-OBI fixtures into testdata/translation/ (needs Kind + Docker)")

// otlpRecorder collects raw OTLP HTTP export bodies by signal path.
type otlpRecorder struct {
	mu     sync.Mutex
	bodies map[string][][]byte // "metrics" | "traces" → decompressed protobuf bodies
}

func (r *otlpRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	kind := ""
	switch req.URL.Path {
	case "/v1/metrics":
		kind = "metrics"
	case "/v1/traces":
		kind = "traces"
	default:
		// OBI also exports logs in some configs; accept and drop.
		w.WriteHeader(http.StatusOK)
		return
	}
	var body io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	b, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.bodies[kind] = append(r.bodies[kind], b)
	r.mu.Unlock()
	// An empty protobuf message is a valid Export*ServiceResponse.
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (r *otlpRecorder) snapshot(kind string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, len(r.bodies[kind]))
	copy(out, r.bodies[kind])
	return out
}

func TestRecordFixtures(t *testing.T) {
	if !*recordFixtures {
		t.Skip("-record not set, skipping fixture recording")
	}

	repoRoot := e2e.GitRoot(t)
	obiImage := e2e.PinnedOBIImage(t, repoRoot)

	// Recorder listens on all interfaces so kind-node containers can
	// reach it via the docker bridge gateway.
	rec := &otlpRecorder{bodies: map[string][][]byte{}}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("recorder listen: %v", err)
	}
	srv := &http.Server{Handler: rec}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	h := e2e.NewHarness(t, "ollie-fixture-recorder")
	t.Cleanup(func() {
		if t.Failed() {
			h.DumpDiagnostics()
		}
	})

	// The docker "kind" network exists once the cluster does; resolve
	// the host-side gateway pods will use to reach the recorder.
	hostIP := kindHostGateway(t)
	endpoint := fmt.Sprintf("http://%s:%d", hostIP, port)
	t.Logf("recorder listening for OBI exports at %s", endpoint)

	h.InstallOllie(repoRoot)

	// Repoint OBI's exporter from the loopback agent to the recorder.
	// The endpoint OBI honors is the one the agent writes into OBI's
	// config file (which overrides OTEL_EXPORTER_OTLP_ENDPOINT — the
	// first recording attempt proved that the hard way), so the knob
	// is the agent's --obi-export-endpoint flag. Everything else
	// (discovery entries, K8s metadata, caps) stays production-shaped.
	// Note: a strategic patch replaces the args list wholesale, so
	// restate the manifest's full set. --controller-addr is dropped
	// deliberately: recording wants the deterministic seed discovery,
	// not controller-driven AllowPod churn.
	h.Kubectl("patch", "daemonset", "ollie-agent", "-n", "ollie-system", "--type=strategic",
		"-p", fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"agent","args":["--otlp-grpc-addr=127.0.0.1:4317","--otlp-http-addr=127.0.0.1:4318","--obi-config=/etc/ollie/obi-config/config.yaml","--obi-instrument-ports=80,443,8080,8443","--obi-export-endpoint=%s"]}]}}}}`, endpoint))
	h.WaitRollout("daemonset", "ollie-agent", "ollie-system", 5*time.Minute)

	h.DeployTestWorkload()

	// Wait until we've captured one representative body per case.
	cases := map[string]func() []byte{
		"http1-recorded": func() []byte {
			return firstMetricsBodyMatching(t, rec, func(name string) bool {
				return name == "http.server.request.duration"
			})
		},
		"l4-recorded": func() []byte {
			return firstMetricsBodyMatching(t, rec, func(name string) bool {
				return strings.Contains(name, "network")
			})
		},
		"traces-http1-recorded": func() []byte {
			return firstTracesBody(t, rec)
		},
	}

	deadline := time.Now().Add(6 * time.Minute)
	captured := map[string][]byte{}
	for len(captured) < len(cases) && time.Now().Before(deadline) {
		for name, pick := range cases {
			if captured[name] != nil {
				continue
			}
			if b := pick(); b != nil {
				t.Logf("captured %s (%d bytes)", name, len(b))
				captured[name] = b
			}
		}
		time.Sleep(5 * time.Second)
	}
	for name := range cases {
		if captured[name] == nil {
			t.Errorf("no body captured for case %s within deadline", name)
		}
	}
	if t.Failed() {
		return
	}

	// Write fixtures + provenance.
	for name, body := range captured {
		kind := "metrics"
		if strings.HasPrefix(name, "traces") {
			kind = "traces"
		}
		body = sanitizeBody(t, kind, body)
		dir := filepath.Join("testdata", "translation", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "input.binpb"), body, 0o644); err != nil {
			t.Fatalf("writing input.binpb: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "kind"), []byte(kind+"\n"), 0o644); err != nil {
			t.Fatalf("writing kind: %v", err)
		}
	}
	prov := fmt.Sprintf(`# Recorded fixture provenance

- OBI image: %s
- Recorded: %s
- Pipeline: TestRecordFixtures (go test ./tests/contract/obi -record) —
  stock k8s/ install on Kind, obi container's OTEL_EXPORTER_OTLP_ENDPOINT
  repointed at an in-test recorder on the host; agnhost echo workload on
  port 8080 with a wget loop client (tests/e2e harness).
- Regenerate: see REGENERATE.md. Re-record on every OBI image bump and
  regenerate goldens with -update in the same PR (ADR-0010 / ADR-0018).
`, obiImage, time.Now().UTC().Format("2006-01-02"))
	if err := os.WriteFile(filepath.Join("testdata", "translation", "RECORDED.md"), []byte(prov), 0o644); err != nil {
		t.Fatalf("writing RECORDED.md: %v", err)
	}
	t.Log("fixtures written; now run: go test ./tests/contract/obi -update")
}

// redactedAttrPrefixes are resource-attribute keys whose values are
// host-environment identity (cloud project, VM hostname, …), not part
// of the OBI wire contract. Values are replaced with "redacted" so
// recordings don't leak the recording machine's environment into the
// repo; keys are kept so the attribute shape stays real.
var redactedAttrPrefixes = []string{"cloud.", "gcp.", "aws.", "azure.", "host."}

func redactKV(attrs []*commonpb.KeyValue) {
	for _, kv := range attrs {
		for _, p := range redactedAttrPrefixes {
			if strings.HasPrefix(kv.Key, p) {
				kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "redacted"}}
				break
			}
		}
	}
}

// sanitizeBody decodes an export body, redacts host-environment
// resource attributes, and re-encodes it.
func sanitizeBody(t *testing.T, kind string, body []byte) []byte {
	t.Helper()
	switch kind {
	case "metrics":
		var req collmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Fatalf("sanitize: unmarshal metrics: %v", err)
		}
		for _, rm := range req.ResourceMetrics {
			if rm.Resource != nil {
				redactKV(rm.Resource.Attributes)
			}
		}
		out, err := proto.Marshal(&req)
		if err != nil {
			t.Fatalf("sanitize: marshal metrics: %v", err)
		}
		return out
	case "traces":
		var req colltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Fatalf("sanitize: unmarshal traces: %v", err)
		}
		for _, rs := range req.ResourceSpans {
			if rs.Resource != nil {
				redactKV(rs.Resource.Attributes)
			}
		}
		out, err := proto.Marshal(&req)
		if err != nil {
			t.Fatalf("sanitize: marshal traces: %v", err)
		}
		return out
	}
	return body
}

// firstMetricsBodyMatching returns the first recorded metrics body
// containing a metric whose name satisfies match, or nil.
func firstMetricsBodyMatching(t *testing.T, rec *otlpRecorder, match func(name string) bool) []byte {
	t.Helper()
	for _, b := range rec.snapshot("metrics") {
		var req collmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			continue
		}
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if match(m.Name) {
						return b
					}
				}
			}
		}
	}
	return nil
}

// firstTracesBody returns the first recorded traces body that contains
// at least one span, or nil.
func firstTracesBody(t *testing.T, rec *otlpRecorder) []byte {
	t.Helper()
	for _, b := range rec.snapshot("traces") {
		var req colltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			continue
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				if len(ss.Spans) > 0 {
					return b
				}
			}
		}
	}
	return nil
}

// kindHostGateway returns the host's IPv4 address on the docker "kind"
// network — the address kind-node containers (and therefore pods, via
// node NAT) can reach the host at.
func kindHostGateway(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "network", "inspect", "kind",
		"-f", `{{range .IPAM.Config}}{{.Gateway}} {{end}}`).Output()
	if err != nil {
		t.Fatalf("inspecting docker kind network (does a kind cluster exist yet? create runs first): %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		ip := net.ParseIP(f)
		if ip != nil && ip.To4() != nil {
			return f
		}
	}
	t.Fatalf("no IPv4 gateway on docker kind network (got %q)", strings.TrimSpace(string(out)))
	return ""
}
