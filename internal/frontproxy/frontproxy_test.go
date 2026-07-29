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

package frontproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// caPEM returns a self-signed CA certificate in PEM form for tests.
func caPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-requestheader-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestParse(t *testing.T) {
	ca := caPEM(t)

	t.Run("valid with allowed names", func(t *testing.T) {
		a, err := parse(map[string]string{
			keyClientCA:     ca,
			keyAllowedNames: `["front-proxy-client","aggregator"]`,
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if a.ClientCAs() == nil {
			t.Fatal("nil CA pool")
		}
		if got := a.AllowedNames(); len(got) != 2 || got[0] != "front-proxy-client" {
			t.Fatalf("allowed names = %v", got)
		}
	})

	t.Run("empty allowed names means any", func(t *testing.T) {
		a, err := parse(map[string]string{keyClientCA: ca})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(a.AllowedNames()) != 0 {
			t.Fatalf("allowed names = %v, want empty", a.AllowedNames())
		}
		if !a.nameAllowed("anything") {
			t.Fatal("empty allowed-names must admit any CN")
		}
	})

	t.Run("missing CA fails closed", func(t *testing.T) {
		if _, err := parse(map[string]string{keyAllowedNames: `[]`}); err == nil {
			t.Fatal("expected error for missing requestheader CA")
		}
	})

	t.Run("garbage PEM fails closed", func(t *testing.T) {
		if _, err := parse(map[string]string{keyClientCA: "not a pem"}); err == nil {
			t.Fatal("expected error for unparseable CA bundle")
		}
	})

	t.Run("bad allowed-names JSON fails", func(t *testing.T) {
		if _, err := parse(map[string]string{keyClientCA: ca, keyAllowedNames: "{"}); err == nil {
			t.Fatal("expected error for bad allowed-names JSON")
		}
	})
}

func TestLoad(t *testing.T) {
	ca := caPEM(t)
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: authNamespace},
		Data:       map[string]string{keyClientCA: ca, keyAllowedNames: `["front-proxy-client"]`},
	})
	a, err := Load(t.Context(), client)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(a.AllowedNames()) != 1 || a.AllowedNames()[0] != "front-proxy-client" {
		t.Fatalf("allowed names = %v", a.AllowedNames())
	}

	// Absent ConfigMap fails closed.
	if _, err := Load(t.Context(), fake.NewSimpleClientset()); err == nil {
		t.Fatal("expected error when the ConfigMap is absent")
	}
}

func TestMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	withCert := func(cn string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/apis/custom.metrics.k8s.io/v1beta1", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: cn},
		}}}
		return r
	}

	cases := []struct {
		name    string
		allowed []string
		req     *http.Request
		want    int
	}{
		{"no client cert rejected", nil, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusUnauthorized},
		{"any cert ok when allowed empty", nil, withCert("whoever"), http.StatusOK},
		{"matching CN ok", []string{"front-proxy-client"}, withCert("front-proxy-client"), http.StatusOK},
		{"non-matching CN forbidden", []string{"front-proxy-client"}, withCert("intruder"), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Authenticator{allowedNames: tc.allowed}
			rec := httptest.NewRecorder()
			a.Middleware(ok).ServeHTTP(rec, tc.req)
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
