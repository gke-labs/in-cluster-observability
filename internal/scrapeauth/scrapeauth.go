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

// Package scrapeauth is an in-process bearer-token authn/authz
// middleware for the agent's HTTP endpoints (#145). It is the
// no-sidecar equivalent of kube-rbac-proxy — the same approach
// controller-runtime's metrics filters take:
//
//  1. TokenReview authenticates the caller's bearer token against
//     the API server.
//  2. SubjectAccessReview authorizes the caller for `get` on the
//     request path as a nonResourceURL. Access is therefore granted
//     by RBAC (see the ollie-metrics-reader ClusterRole in
//     k8s/rbac.yaml), which only RBAC-granters can extend — not by
//     anything a workload can label or mount itself.
//
// Decisions are cached per (token, path) with short TTLs so
// steady-state scraping costs one TokenReview+SAR pair per TTL, not
// per scrape. Failures are closed: an unreachable API server yields
// 500, never an unauthenticated pass-through.
//
// The middleware is keyed by request path, not hard-coded to
// /metrics, so future endpoints (v0.5 query API) can mount it with
// their own RBAC nonResourceURL grants.
package scrapeauth

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultAllowedTTL = 2 * time.Minute // kubelet webhook-cache precedent
	defaultDeniedTTL  = 10 * time.Second
	maxCacheEntries   = 4096
)

// Config configures the middleware.
type Config struct {
	// Client performs the TokenReview / SubjectAccessReview calls.
	Client kubernetes.Interface

	// Audiences, when non-empty, requires tokens bound to at least
	// one of these audiences (projected ServiceAccount tokens).
	// Empty accepts standard API-server-audience tokens — managed
	// collectors (GMP) can't mint custom audiences.
	Audiences []string

	// ExemptLoopback skips auth for requests originating from a
	// loopback address. Only processes inside the pod's own network
	// namespace (sibling containers, kubectl debug ephemeral
	// containers) can source loopback; other pods cannot. Keeps the
	// documented pod-internal smoke flow working.
	ExemptLoopback bool

	// AllowedTTL / DeniedTTL override the decision-cache lifetimes.
	AllowedTTL time.Duration
	DeniedTTL  time.Duration
}

// Middleware authenticates and authorizes bearer tokens. Create with
// New; wrap handlers with Wrap.
type Middleware struct {
	cfg Config

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	status  int // http.StatusOK if allowed, else the denial status
	expires time.Time
}

// New returns a Middleware backed by the given config. Config.Client
// is required.
func New(cfg Config) *Middleware {
	if cfg.AllowedTTL <= 0 {
		cfg.AllowedTTL = defaultAllowedTTL
	}
	if cfg.DeniedTTL <= 0 {
		cfg.DeniedTTL = defaultDeniedTTL
	}
	return &Middleware{cfg: cfg, cache: make(map[string]cacheEntry)}
}

// Wrap returns a handler that authenticates + authorizes each request
// before delegating to next.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.cfg.ExemptLoopback && isLoopback(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}

		token := bearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ollie"`)
			http.Error(w, "bearer token required", http.StatusUnauthorized)
			return
		}

		key := cacheKey(token, r.URL.Path)
		if status, ok := m.cached(key); ok {
			if status == http.StatusOK {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, http.StatusText(status), status)
			return
		}

		status := m.review(r, token)
		switch status {
		case http.StatusOK:
			m.store(key, status, m.cfg.AllowedTTL)
			next.ServeHTTP(w, r)
		case http.StatusInternalServerError:
			// API-server hiccup: fail closed but don't cache, the
			// next scrape should retry the review.
			http.Error(w, "authentication unavailable", status)
		default:
			m.store(key, status, m.cfg.DeniedTTL)
			http.Error(w, http.StatusText(status), status)
		}
	})
}

// review performs the uncached TokenReview + SubjectAccessReview
// round-trips and maps the outcome to an HTTP status.
func (m *Middleware) review(r *http.Request, token string) int {
	tr, err := m.cfg.Client.AuthenticationV1().TokenReviews().Create(
		r.Context(),
		&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{
			Token:     token,
			Audiences: m.cfg.Audiences,
		}},
		metav1.CreateOptions{},
	)
	if err != nil {
		return http.StatusInternalServerError
	}
	if !tr.Status.Authenticated {
		return http.StatusUnauthorized
	}

	extra := make(map[string]authzv1.ExtraValue, len(tr.Status.User.Extra))
	for k, v := range tr.Status.User.Extra {
		extra[k] = authzv1.ExtraValue(v)
	}
	sar, err := m.cfg.Client.AuthorizationV1().SubjectAccessReviews().Create(
		r.Context(),
		&authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
			User:   tr.Status.User.Username,
			Groups: tr.Status.User.Groups,
			UID:    tr.Status.User.UID,
			Extra:  extra,
			NonResourceAttributes: &authzv1.NonResourceAttributes{
				Path: r.URL.Path,
				Verb: strings.ToLower(r.Method),
			},
		}},
		metav1.CreateOptions{},
	)
	if err != nil {
		return http.StatusInternalServerError
	}
	if !sar.Status.Allowed {
		return http.StatusForbidden
	}
	return http.StatusOK
}

func (m *Middleware) cached(key string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[key]
	if !ok || time.Now().After(e.expires) {
		return 0, false
	}
	return e.status, true
}

func (m *Middleware) store(key string, status int, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.cache) >= maxCacheEntries {
		// Cheap bound: drop expired entries; if everything is live,
		// reset. Entries are re-derived on the next request.
		now := time.Now()
		for k, e := range m.cache {
			if now.After(e.expires) {
				delete(m.cache, k)
			}
		}
		if len(m.cache) >= maxCacheEntries {
			m.cache = make(map[string]cacheEntry)
		}
	}
	m.cache[key] = cacheEntry{status: status, expires: time.Now().Add(ttl)}
}

// cacheKey hashes the token so raw credentials never sit in memory
// beyond the request lifetime.
func cacheKey(token, path string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]) + "|" + path
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
