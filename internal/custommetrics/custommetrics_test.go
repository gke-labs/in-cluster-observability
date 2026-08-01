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

package custommetrics

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql"
)

// fakeEval records the rendered PromQL and returns a fixed vector.
type fakeEval struct {
	lastExpr string
	vec      promql.Vector
	err      error
}

func (f *fakeEval) InstantVector(_ context.Context, expr string, _ time.Time) (promql.Vector, error) {
	f.lastExpr = expr
	return f.vec, f.err
}

func newHandler(t *testing.T, eval Evaluator, configYAML string) *Handler {
	t.Helper()
	path := ""
	if configYAML != "" {
		path = filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := New(Config{Evaluator: eval, ConfigPath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func do(t *testing.T, h *Handler, path string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestDiscovery(t *testing.T) {
	h := newHandler(t, &fakeEval{}, "")
	code, body := do(t, h, basePath)
	if code != http.StatusOK {
		t.Fatalf("discovery code = %d", code)
	}
	var list struct {
		Kind         string `json:"kind"`
		GroupVersion string `json:"groupVersion"`
		Resources    []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if list.Kind != "APIResourceList" || list.GroupVersion != GroupVersion {
		t.Fatalf("list header = %+v", list)
	}
	found := false
	for _, r := range list.Resources {
		if r.Name == "deployments/qps" && r.Kind == "MetricValueList" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deployments/qps missing from discovery: %+v", list.Resources)
	}
}

func TestObjectMetric(t *testing.T) {
	eval := &fakeEval{vec: promql.Vector{{F: 4.25}}}
	h := newHandler(t, eval, "")

	// Both the kubectl bare-plural form and the HPA's grouped form.
	for _, res := range []string{"deployments", "deployments.apps"} {
		code, body := do(t, h, basePath+"/namespaces/shop/"+res+"/backend/qps")
		if code != http.StatusOK {
			t.Fatalf("%s: code = %d body=%s", res, code, body)
		}
		var mvl MetricValueList
		if err := json.Unmarshal(body, &mvl); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if mvl.Kind != "MetricValueList" || mvl.APIVersion != GroupVersion {
			t.Fatalf("typemeta = %+v", mvl.TypeMeta)
		}
		if len(mvl.Items) != 1 {
			t.Fatalf("items = %d", len(mvl.Items))
		}
		it := mvl.Items[0]
		if it.MetricName != "qps" || it.DescribedObject.Kind != "Deployment" ||
			it.DescribedObject.Namespace != "shop" || it.DescribedObject.Name != "backend" {
			t.Fatalf("item = %+v", it)
		}
		if it.Value.MilliValue() != 4250 {
			t.Fatalf("value = %s, want 4250m", it.Value.String())
		}
	}

	// The rendered PromQL carries the namespace + deployment label
	// selector.
	if !strings.Contains(eval.lastExpr, `k8s_namespace_name="shop"`) ||
		!strings.Contains(eval.lastExpr, `k8s_deployment_name="backend"`) {
		t.Fatalf("rendered expr = %s", eval.lastExpr)
	}
}

func TestErrors(t *testing.T) {
	h := newHandler(t, &fakeEval{vec: promql.Vector{}}, "")

	for path, want := range map[string]int{
		basePath + "/namespaces/ns/gadgets/x/qps":         http.StatusNotFound, // unknown resource
		basePath + "/namespaces/ns/pods/x/nope":           http.StatusNotFound, // unknown metric
		basePath + "/namespaces/ns/pods/*/qps":            http.StatusNotImplemented,
		basePath + "/namespaces/ns/deployments/empty/qps": http.StatusNotFound, // no series
	} {
		code, body := do(t, h, path)
		if code != want {
			t.Errorf("%s: code = %d (want %d) body=%s", path, code, want, body)
		}
		if !strings.Contains(string(body), `"kind":"Status"`) {
			t.Errorf("%s: error body not a metav1.Status: %s", path, body)
		}
	}
}

// The HPA's spec.metrics[].object.metric.selector arrives as the
// metricLabelSelector query parameter and must scope the PromQL —
// ignoring it returned the unfiltered aggregate (#185).
func TestMetricLabelSelector(t *testing.T) {
	eval := &fakeEval{vec: promql.Vector{{F: 1}}}
	h := newHandler(t, eval, "")

	base := basePath + "/namespaces/shop/deployments/backend/qps"
	for _, tc := range []struct {
		sel  string
		want []string
	}{
		{"tier=frontend", []string{`tier="frontend"`}},
		{"tier!=canary", []string{`tier!="canary"`}},
		{"tier in (a,b)", []string{`tier=~"a|b"`}},
		{"tier notin (a.b)", []string{`tier!~"a\\.b"`}},                  // regex metachars quoted (PromQL-escaped)
		{"k8s.container.name=api", []string{`k8s_container_name="api"`}}, // dotted alias
		{"tier", []string{`tier!=""`}},
		{"!tier", []string{`tier=""`}},
	} {
		code, body := do(t, h, base+"?metricLabelSelector="+url.QueryEscape(tc.sel))
		if code != http.StatusOK {
			t.Fatalf("%s: code = %d body=%s", tc.sel, code, body)
		}
		for _, want := range tc.want {
			if !strings.Contains(eval.lastExpr, want) {
				t.Errorf("%s: rendered expr %s missing %s", tc.sel, eval.lastExpr, want)
			}
		}
	}

	// Unparseable selectors are rejected loudly, never silently
	// unfiltered.
	code, body := do(t, h, base+"?metricLabelSelector="+url.QueryEscape("a=b,==="))
	if code != http.StatusBadRequest {
		t.Fatalf("bad selector: code = %d (want 400) body=%s", code, body)
	}
}

// The namespace pseudo-resource has its own (shorter) URL shape and
// was advertised in discovery with no route behind it (#193).
func TestNamespaceMetric(t *testing.T) {
	eval := &fakeEval{vec: promql.Vector{{F: 2}}}
	h := newHandler(t, eval, "")
	code, body := do(t, h, basePath+"/namespaces/shop/metrics/qps")
	if code != http.StatusOK {
		t.Fatalf("code = %d body=%s", code, body)
	}
	var mvl MetricValueList
	if err := json.Unmarshal(body, &mvl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	it := mvl.Items[0]
	if it.DescribedObject.Kind != "Namespace" || it.DescribedObject.Name != "shop" || it.DescribedObject.Namespace != "" {
		t.Fatalf("describedObject = %+v", it.DescribedObject)
	}
	if !strings.Contains(eval.lastExpr, `k8s_namespace_name="shop"`) {
		t.Fatalf("expr = %s", eval.lastExpr)
	}
}

// services is deliberately not served by default (#193): OBI's
// service_name is OTel attribution, not the K8s Service object.
func TestServicesNotServedByDefault(t *testing.T) {
	h := newHandler(t, &fakeEval{vec: promql.Vector{{F: 1}}}, "")
	code, _ := do(t, h, basePath+"/namespaces/ns/services/svc/qps")
	if code != http.StatusNotFound {
		t.Fatalf("services: code = %d, want 404", code)
	}
	// The ConfigMap overlay opts back in.
	h2 := newHandler(t, &fakeEval{vec: promql.Vector{{F: 1}}}, "resources:\n  services: service_name\n")
	code, _ = do(t, h2, basePath+"/namespaces/ns/services/svc/qps")
	if code != http.StatusOK {
		t.Fatalf("services via overlay: code = %d, want 200", code)
	}
}

func TestNonFiniteValue(t *testing.T) {
	// histogram_quantile over idle traffic yields NaN; arithmetic
	// templates can overflow to ±Inf. Either must be treated as "no
	// value" (404) so the HPA holds its replica count rather than
	// scaling on a garbage int64 from NewMilliQuantity.
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		eval := &fakeEval{vec: promql.Vector{{F: f}}}
		h := newHandler(t, eval, "")
		code, body := do(t, h, basePath+"/namespaces/shop/deployments/backend/latency_p99")
		if code != http.StatusNotFound {
			t.Fatalf("value %v: code = %d (want 404) body=%s", f, code, body)
		}
		if !strings.Contains(string(body), `"kind":"Status"`) {
			t.Fatalf("value %v: error body not a metav1.Status: %s", f, body)
		}
	}
}

func TestConfigOverlay(t *testing.T) {
	eval := &fakeEval{vec: promql.Vector{{F: 1}}}
	h := newHandler(t, eval, `
metrics:
  error_rate: 'sum(rate(http_server_request_duration_count{ {{.Selector}} ,http_response_status_code=~"5.."}[1m]))'
  latency_p50: ""
resources:
  gadgets: gadget_name
`)

	// New metric + new resource work.
	code, _ := do(t, h, basePath+"/namespaces/ns/gadgets/g1/error_rate")
	if code != http.StatusOK {
		t.Fatalf("overlay metric code = %d", code)
	}
	if !strings.Contains(eval.lastExpr, `gadget_name="g1"`) {
		t.Fatalf("expr = %s", eval.lastExpr)
	}
	// Emptied default is gone.
	code, _ = do(t, h, basePath+"/namespaces/ns/pods/p/latency_p50")
	if code != http.StatusNotFound {
		t.Fatalf("removed default code = %d, want 404", code)
	}
	// Untouched default survives.
	code, _ = do(t, h, basePath+"/namespaces/ns/pods/p/qps")
	if code != http.StatusOK {
		t.Fatalf("default qps code = %d", code)
	}
}

func TestSelfSignedCert(t *testing.T) {
	cert, err := SelfSignedCert([]string{"ollie-query.ollie-system.svc", "localhost"})
	if err != nil {
		t.Fatalf("SelfSignedCert: %v", err)
	}
	if len(cert.Certificate) != 1 || cert.PrivateKey == nil {
		t.Fatal("incomplete certificate")
	}
}
