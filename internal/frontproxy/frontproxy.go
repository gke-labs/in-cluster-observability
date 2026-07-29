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

// Package frontproxy authenticates the Kubernetes aggregation layer to
// an aggregated API server (the custom.metrics.k8s.io backend, #96).
//
// The kube-apiserver does not forward the end user's bearer token when
// it proxies an aggregated API request; it authenticates to the backend
// with its own front-proxy CLIENT CERTIFICATE (--proxy-client-cert-file,
// signed by the cluster's requestheader CA) and passes the original
// caller's identity in X-Remote-User / X-Remote-Group headers. So the
// backend cannot reuse the bearer-token TokenReview posture the agent's
// scrape endpoints use (internal/scrapeauth); it must instead pin TLS
// to the requestheader CA.
//
// This package loads the requestheader trust anchors published by the
// API server in the kube-system/extension-apiserver-authentication
// ConfigMap and exposes:
//
//   - ClientCAs(): the CA pool to set as tls.Config.ClientCAs with
//     ClientAuth = RequireAndVerifyClientCert. This is the load-bearing
//     control: only a client holding a cert signed by the requestheader
//     CA — in practice only the kube-apiserver's aggregator — can
//     complete the TLS handshake. Any other pod is refused at the
//     transport layer, which closes the "any pod reads cluster-wide
//     metrics unauthenticated" hole.
//
//   - Middleware(): a belt-and-suspenders check that the verified client
//     certificate's Common Name is in requestheader-allowed-names (when
//     that list is non-empty), rejecting an otherwise-valid cert that is
//     not the designated aggregator identity.
//
// Per-user delegated authorization (a SubjectAccessReview on the
// X-Remote-User the aggregator forwards) is deferred to the v0.6 TLS/
// authz milestone; the kube-apiserver already enforces the HPA
// controller's RBAC on the custom-metrics resource before it proxies,
// so requiring the aggregator's client cert is a complete fix for the
// unauthenticated-access defect on its own.
package frontproxy

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// authNamespace / authConfigMap locate the API server's published
	// requestheader configuration.
	authNamespace = "kube-system"
	authConfigMap = "extension-apiserver-authentication"

	// keyClientCA / keyAllowedNames are the ConfigMap keys we read.
	keyClientCA     = "requestheader-client-ca-file"
	keyAllowedNames = "requestheader-allowed-names"
)

// Authenticator holds the requestheader trust anchors.
type Authenticator struct {
	caPool       *x509.CertPool
	allowedNames []string // empty ⇒ any cert signed by the CA is accepted
}

// Load fetches the extension-apiserver-authentication ConfigMap and
// parses the requestheader CA bundle + allowed-names. It fails closed:
// a missing ConfigMap, absent CA bundle, or unparseable PEM is an
// error, never a silent open.
func Load(ctx context.Context, client kubernetes.Interface) (*Authenticator, error) {
	cm, err := client.CoreV1().ConfigMaps(authNamespace).Get(ctx, authConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("frontproxy: read %s/%s: %w", authNamespace, authConfigMap, err)
	}
	return parse(cm.Data)
}

// parse builds an Authenticator from the ConfigMap data map. Split out
// from Load so it is testable without a cluster.
func parse(data map[string]string) (*Authenticator, error) {
	caPEM, ok := data[keyClientCA]
	if !ok || caPEM == "" {
		return nil, fmt.Errorf("frontproxy: %q is empty in %s (aggregation not configured?); refusing to serve custom metrics without client-cert trust", keyClientCA, authConfigMap)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, fmt.Errorf("frontproxy: %q contains no usable PEM certificates", keyClientCA)
	}

	var allowed []string
	if raw := data[keyAllowedNames]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &allowed); err != nil {
			return nil, fmt.Errorf("frontproxy: parse %q: %w", keyAllowedNames, err)
		}
	}
	return &Authenticator{caPool: pool, allowedNames: allowed}, nil
}

// ClientCAs returns the requestheader CA pool. Set it as
// tls.Config.ClientCAs with ClientAuth = RequireAndVerifyClientCert.
func (a *Authenticator) ClientCAs() *x509.CertPool { return a.caPool }

// AllowedNames returns the configured requestheader-allowed-names
// (empty means any cert signed by the CA is accepted).
func (a *Authenticator) AllowedNames() []string {
	return append([]string(nil), a.allowedNames...)
}

// Middleware enforces the allowed-names check on top of the TLS-layer
// client-cert verification. TLS (RequireAndVerifyClientCert against
// ClientCAs) has already proven the presented cert chains to the
// requestheader CA by the time a request reaches here; this additionally
// requires the cert's Common Name to be in requestheader-allowed-names
// when that list is set, so only the designated aggregator identity — not
// any holder of a CA-signed cert — is admitted.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			// With RequireAndVerifyClientCert this is unreachable over
			// TLS, but guard anyway so a misconfigured plaintext mount
			// can never serve data.
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if !a.nameAllowed(r.TLS.PeerCertificates[0].Subject.CommonName) {
			http.Error(w, "client certificate is not an allowed aggregator identity", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) nameAllowed(cn string) bool {
	if len(a.allowedNames) == 0 {
		return true // any cert signed by the requestheader CA
	}
	for _, n := range a.allowedNames {
		if n == cn {
			return true
		}
	}
	return false
}
