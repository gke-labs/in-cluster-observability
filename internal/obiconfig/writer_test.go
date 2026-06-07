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

package obiconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gke-labs/in-cluster-observability/internal/obiconfig"
)

func TestDefaultFile_ShapeMatchesADR(t *testing.T) {
	f := obiconfig.DefaultFile("127.0.0.1:4317")
	if f.OtelMetricsExport.Endpoint != "127.0.0.1:4317" {
		t.Errorf("metrics endpoint = %q; want 127.0.0.1:4317", f.OtelMetricsExport.Endpoint)
	}
	if f.OtelTracesExport.Endpoint != "127.0.0.1:4317" {
		t.Errorf("traces endpoint = %q; want 127.0.0.1:4317", f.OtelTracesExport.Endpoint)
	}
	// ADR-0021 (supersedes ADR-0017.4): OBI is the single source of
	// K8s identity on captured events; the informer must default on.
	if !f.Attributes.Kubernetes.Enable {
		t.Error("Attributes.Kubernetes.Enable defaulted false; ADR-0021 says on (OBI owns enrichment)")
	}
	if f.Routes == nil || f.Routes.Unmatched != "wildcard" {
		t.Errorf("routes.unmatched should default to wildcard; got %+v", f.Routes)
	}
}

func TestWriter_RejectsBadInputs(t *testing.T) {
	if _, err := obiconfig.NewWriter(""); err == nil {
		t.Fatal("empty path should error")
	}
	if _, err := obiconfig.NewWriter("/this/parent/does/not/exist/x.yaml"); err == nil {
		t.Fatal("missing parent directory should error")
	}
}

func TestWriter_WritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	w, err := obiconfig.NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	changed, err := w.Write(obiconfig.DefaultFile("127.0.0.1:4317"))
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !changed {
		t.Error("first write should report changed=true")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	content := string(got)
	if !strings.Contains(content, "otel_metrics_export") {
		t.Errorf("expected otel_metrics_export in output; got:\n%s", content)
	}
	if !strings.Contains(content, "endpoint: 127.0.0.1:4317") {
		t.Errorf("expected endpoint line in output; got:\n%s", content)
	}
	if !strings.Contains(content, "enable: true") {
		t.Errorf("expected K8s attrs enabled (ADR-0021); got:\n%s", content)
	}
}

func TestWriter_ShortCircuitsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	w, _ := obiconfig.NewWriter(path)

	f := obiconfig.DefaultFile("127.0.0.1:4317")
	if changed, _ := w.Write(f); !changed {
		t.Fatal("first write should be changed")
	}
	if changed, _ := w.Write(f); changed {
		t.Fatal("repeat write with identical content should be no-op")
	}

	f.Discovery.Instrument = []obiconfig.Instrument{{Name: "test", OpenPorts: "8080"}}
	if changed, _ := w.Write(f); !changed {
		t.Fatal("write with new content should be changed")
	}
}

func TestWriter_DiscoveryInstrumentShape(t *testing.T) {
	// Verifies the on-disk YAML uses OBI v0.9's selector key
	// (`discovery.instrument`, not `services`) and emits open_ports as
	// a string scalar — not a YAML list of ints. OBI silently ignores
	// entries under the wrong key, which is painful to debug.
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	w, _ := obiconfig.NewWriter(path)

	f := obiconfig.DefaultFile("http://127.0.0.1:4318")
	f.Discovery.Instrument = []obiconfig.Instrument{{
		Name:      "smoke",
		OpenPorts: "80,8080",
	}}
	if _, err := w.Write(f); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	content := string(got)
	if !strings.Contains(content, "instrument:") {
		t.Errorf("expected `instrument:` selector key; got:\n%s", content)
	}
	if strings.Contains(content, "services:") {
		t.Errorf("did not expect legacy `services:` key; got:\n%s", content)
	}
	if !strings.Contains(content, "open_ports: 80,8080") {
		t.Errorf("expected `open_ports: 80,8080` as string scalar; got:\n%s", content)
	}
}

func TestWriter_K8sMetadataInstrument(t *testing.T) {
	// Verifies the v0.4 controller-driven path's K8s metadata
	// selectors land inlined at the top of the Instrument entry
	// (OBI's MetadataGlobMap is `yaml:",inline"`), not nested under
	// some `metadata:` sub-key.
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	w, _ := obiconfig.NewWriter(path)

	f := obiconfig.DefaultFile("http://127.0.0.1:4318")
	f.Discovery.Instrument = []obiconfig.Instrument{{
		Name:         "pod-abc123",
		K8sPodName:   "nginx-567b68cc5f-6mggl",
		K8sNamespace: "demo",
		OpenPorts:    "80",
	}}
	if _, err := w.Write(f); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(path)
	content := string(got)
	if !strings.Contains(content, "k8s_pod_name: nginx-567b68cc5f-6mggl") {
		t.Errorf("expected `k8s_pod_name:` at top of entry; got:\n%s", content)
	}
	if !strings.Contains(content, "k8s_namespace: demo") {
		t.Errorf("expected `k8s_namespace:` at top of entry; got:\n%s", content)
	}
	// Make sure they're NOT nested under a `metadata:` subkey (a
	// past bug shape we want a regression guard for).
	if strings.Contains(content, "metadata:") {
		t.Errorf("k8s_* keys should be inline, not under a metadata: subkey; got:\n%s", content)
	}
}

func TestWriter_FileIsWorldReadable(t *testing.T) {
	// The OBI sibling container reads this file as a different uid
	// than the agent container; world-readable (0644) avoids the
	// permission-denied class of error in mixed-uid pods.
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	w, _ := obiconfig.NewWriter(path)
	if _, err := w.Write(obiconfig.DefaultFile("127.0.0.1:4317")); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("file mode = %#o; want 0644 (world-readable)", mode)
	}
}

func TestWriter_NoTempArtifactsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obi.yaml")
	w, _ := obiconfig.NewWriter(path)
	if _, err := w.Write(obiconfig.DefaultFile("127.0.0.1:4317")); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".obi-config-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
