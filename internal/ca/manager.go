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

package ca

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Manager owns the cluster-side lifecycle of the self-managed CA
// (ADR-0028). It runs only on the elected controller leader, so it is
// the single writer of the CA Secret, the serving-cert Secret, and the
// APIService caBundle — no two replicas can race.
//
// Each Reconcile pass, in order:
//  1. ensure the CA Secret exists (create once; never auto-rotated here);
//  2. ensure the query serving-cert Secret is present, issued by the CA,
//     and covers the wanted SANs (re-issue on drift or near-expiry);
//  3. gated commit: the aggregation API rejects a caBundle that coexists
//     with insecureSkipTLSVerify=true, so both fields move together in a
//     single atomic patch. We keep the shipped bootstrap posture
//     (insecureSkipTLSVerify=true, empty caBundle — which never breaks
//     HPAs) until EVERY ready ollie-query endpoint presents a leaf that
//     chains to the current CA, then write the caBundle and clear the
//     flag at once. This is the HPA-takedown guard: committing while any
//     replica still serves the old self-signed cert would make the
//     aggregator reject the backend and mark the APIService Unavailable.
type Manager struct {
	Clientset kubernetes.Interface
	APISvc    APIServiceStore

	Namespace       string   // install namespace, e.g. ollie-system
	CASecret        string   // e.g. ollie-ca
	ServingSecret   string   // e.g. ollie-query-serving
	ServingDNSNames []string // SANs for the query serving cert
	QueryService    string   // Service whose endpoints back :6443, e.g. ollie-query
	TLSPort         int      // query custom-metrics port, e.g. 6443

	// Agent serving cert (intra-ollie TLS, ADR-0029/#197): one shared
	// keypair for every agent's :9091/:9092 listener, mounted by the
	// DaemonSet. Clients dial pod IPs but verify against the headless-
	// Service DNS SANs via tls.Config.ServerName. Empty
	// AgentServingSecret disables issuance.
	AgentServingSecret   string
	AgentServingDNSNames []string

	CALifetime      time.Duration
	ServingLifetime time.Duration
	RenewBefore     time.Duration // re-issue serving cert this long before expiry
	ResyncInterval  time.Duration // how often to re-run Reconcile (default 1m)

	Logger *slog.Logger

	// now and probeLeaf are injectable for tests; nil uses real
	// implementations.
	now       func() time.Time
	probeLeaf func(ctx context.Context, addr string) (leafDER []byte, err error)
}

// APIServiceStore abstracts the APIService writes so the orchestration
// is unit-testable without a real aggregation API. The concrete
// implementation (NewDynamicAPIServiceStore) patches via the dynamic
// client, avoiding a kube-aggregator dependency.
type APIServiceStore interface {
	// Get returns the current caBundle and insecureSkipTLSVerify.
	Get(ctx context.Context) (caBundle []byte, insecure bool, err error)
	// Commit atomically sets spec.caBundle=caPEM and
	// spec.insecureSkipTLSVerify=false in a single patch. The aggregation
	// API rejects a non-empty caBundle alongside insecureSkipTLSVerify=true,
	// so the two fields must transition together. The caller guarantees
	// every serving endpoint already presents a caPEM-signed leaf.
	Commit(ctx context.Context, caPEM []byte) error
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// Reconcile runs one full pass. It is idempotent and safe to call
// repeatedly.
func (m *Manager) Reconcile(ctx context.Context) error {
	authority, err := m.ensureCA(ctx)
	if err != nil {
		return fmt.Errorf("ensure CA: %w", err)
	}
	if err := m.ensureServingCert(ctx, authority, m.ServingSecret, m.ServingDNSNames, "query"); err != nil {
		return fmt.Errorf("ensure query serving cert: %w", err)
	}
	if m.AgentServingSecret != "" {
		if err := m.ensureServingCert(ctx, authority, m.AgentServingSecret, m.AgentServingDNSNames, "agent"); err != nil {
			return fmt.Errorf("ensure agent serving cert: %w", err)
		}
	}
	if err := m.reconcileAPIService(ctx, authority); err != nil {
		return fmt.Errorf("reconcile APIService: %w", err)
	}
	return nil
}

// ensureCA loads the CA from its Secret, minting and persisting a fresh
// one only if the Secret is absent.
func (m *Manager) ensureCA(ctx context.Context) (*CA, error) {
	sec, err := m.Clientset.CoreV1().Secrets(m.Namespace).Get(ctx, m.CASecret, metav1.GetOptions{})
	switch {
	case err == nil:
		authority, perr := Parse(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey])
		if perr != nil {
			// A corrupt CA Secret is not something we silently
			// overwrite: the caBundle already distributed may still
			// trust the old key, so re-minting would break every
			// consumer. Surface it for an operator.
			return nil, fmt.Errorf("existing %s Secret is unusable (refusing to overwrite): %w", m.CASecret, perr)
		}
		return authority, nil
	case apierrors.IsNotFound(err):
		lifetime := m.CALifetime
		if lifetime == 0 {
			lifetime = CADefaultLifetime
		}
		authority, mErr := NewCA(m.clock(), lifetime)
		if mErr != nil {
			return nil, mErr
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      m.CASecret,
				Namespace: m.Namespace,
				Labels:    ollieLabels("controller"),
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{
				corev1.TLSCertKey:       authority.CertPEM(),
				corev1.TLSPrivateKeyKey: authority.KeyPEM(),
			},
		}
		if _, cErr := m.Clientset.CoreV1().Secrets(m.Namespace).Create(ctx, sec, metav1.CreateOptions{}); cErr != nil {
			if apierrors.IsAlreadyExists(cErr) {
				// Lost a race (should not happen under leader election,
				// but be safe): re-read.
				return m.ensureCA(ctx)
			}
			return nil, fmt.Errorf("create %s: %w", m.CASecret, cErr)
		}
		m.log().Info("minted self-managed CA", "secret", m.CASecret, "notAfter", authority.NotAfter())
		return authority, nil
	default:
		return nil, err
	}
}

// ensureServingCert makes a serving-cert Secret match the CA and the
// wanted SANs, re-issuing on drift or when it is within RenewBefore of
// expiry. component labels the Secret and log lines ("query", "agent").
func (m *Manager) ensureServingCert(ctx context.Context, authority *CA, secretName string, dnsNames []string, component string) error {
	now := m.clock()
	servingLifetime := m.ServingLifetime
	if servingLifetime == 0 {
		servingLifetime = ServingDefaultLifetime
	}
	renewBefore := m.RenewBefore
	if renewBefore == 0 {
		renewBefore = servingLifetime / 3
	}

	sec, err := m.Clientset.CoreV1().Secrets(m.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	exists := err == nil

	if exists {
		cur := sec.Data[corev1.TLSCertKey]
		if ServingCertMatches(cur, authority.CertPEM(), dnsNames, now) {
			if exp, eErr := ServingCertExpiry(cur); eErr == nil && exp.Sub(now) > renewBefore {
				return nil // healthy; nothing to do
			}
		}
	}

	certPEM, keyPEM, err := authority.IssueServingCert(dnsNames, now, servingLifetime)
	if err != nil {
		return err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: m.Namespace,
			Labels:    ollieLabels(component),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
			// Distribute the CA too, so consumers that verify a peer
			// (intra-ollie TLS, #197) can mount ca.crt from the same
			// Secret.
			"ca.crt": authority.CertPEM(),
		},
	}
	if exists {
		sec.Data = desired.Data
		sec.Type = desired.Type
		if _, uErr := m.Clientset.CoreV1().Secrets(m.Namespace).Update(ctx, sec, metav1.UpdateOptions{}); uErr != nil {
			return fmt.Errorf("update %s: %w", secretName, uErr)
		}
		m.log().Info("re-issued serving cert", "component", component, "secret", secretName)
		return nil
	}
	if _, cErr := m.Clientset.CoreV1().Secrets(m.Namespace).Create(ctx, desired, metav1.CreateOptions{}); cErr != nil {
		return fmt.Errorf("create %s: %w", secretName, cErr)
	}
	m.log().Info("issued serving cert", "component", component, "secret", secretName)
	return nil
}

// reconcileAPIService drives the APIService to the verified-TLS target
// state (caBundle=CA, insecureSkipTLSVerify=false) in a single atomic
// patch, but only once every ready query endpoint serves a CA-signed
// leaf. The aggregation API forbids a non-empty caBundle while
// insecureSkipTLSVerify is true, so the two cannot be split across
// passes: until the gate opens we leave the shipped bootstrap posture
// (skip-verify on, empty caBundle) untouched — that never breaks an HPA.
// If any endpoint fails the check (or none are ready yet), it leaves the
// APIService as-is and retries next pass.
func (m *Manager) reconcileAPIService(ctx context.Context, authority *CA) error {
	curBundle, insecure, err := m.APISvc.Get(ctx)
	if err != nil {
		return err
	}
	desired := authority.CertPEM()
	if !insecure && string(curBundle) == string(desired) {
		return nil // already in the verified-TLS target state
	}
	addrs, err := m.readyEndpoints(ctx)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		m.log().Info("apiservice gate: no ready query endpoints yet; leaving bootstrap TLS posture")
		return nil
	}
	now := m.clock()
	for _, addr := range addrs {
		leaf, pErr := m.probe(ctx, addr)
		if pErr != nil {
			m.log().Info("apiservice gate: endpoint not ready for TLS verification", "addr", addr, "err", pErr)
			return nil
		}
		if vErr := VerifyServedBy(leaf, desired, "", now); vErr != nil {
			m.log().Info("apiservice gate: endpoint still serving a non-CA cert", "addr", addr, "err", vErr)
			return nil
		}
	}
	if err := m.APISvc.Commit(ctx, desired); err != nil {
		return err
	}
	m.log().Info("all query endpoints serve the self-managed CA; committed caBundle and enabled TLS verification", "endpoints", len(addrs))
	return nil
}

// readyEndpoints lists the ready pod addresses backing the query Service
// on the TLS port.
func (m *Manager) readyEndpoints(ctx context.Context) ([]string, error) {
	ep, err := m.Clientset.CoreV1().Endpoints(m.Namespace).Get(ctx, m.QueryService, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var addrs []string
	for _, sub := range ep.Subsets {
		for _, a := range sub.Addresses { // ready addresses only
			addrs = append(addrs, net.JoinHostPort(a.IP, fmt.Sprintf("%d", m.TLSPort)))
		}
	}
	return addrs, nil
}

// probe dials addr and returns the server's leaf certificate DER. It
// forces TLS 1.3 so the handshake completes and exposes the server cert
// even though :6443 requires a client certificate — under TLS 1.3 the
// server's client-cert check is post-handshake, so a certless probe
// still observes the served cert (and we never send a request).
func (m *Manager) probe(ctx context.Context, addr string) ([]byte, error) {
	if m.probeLeaf != nil {
		return m.probeLeaf(ctx, addr)
	}
	dialer := &tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // we verify the chain ourselves against our CA
		MinVersion:         tls.VersionTLS13,
	}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return nil, fmt.Errorf("no server certificate presented")
	}
	return state.PeerCertificates[0].Raw, nil
}

// Start runs Reconcile immediately and then on a ticker until ctx is
// done. It implements controller-runtime's manager.Runnable.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.Reconcile(ctx); err != nil {
		m.log().Error("CA reconcile failed", "err", err)
	}
	// Resync often: on a fresh install the flip gate must retry until
	// kubelet has remounted the serving Secret into the query pods and
	// they have reloaded it (tens of seconds), which the initial pass
	// almost never catches. Post-flip the pass is a handful of cheap
	// Gets. Rotation/renewal decisions ride the same loop.
	interval := m.ResyncInterval
	if interval == 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := m.Reconcile(ctx); err != nil {
				m.log().Error("CA reconcile failed", "err", err)
			}
		}
	}
}

// NeedLeaderElection makes controller-runtime run this Runnable only on
// the elected leader — the single-writer guarantee.
func (m *Manager) NeedLeaderElection() bool { return true }

func ollieLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "ollie",
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "ollie-controller",
	}
}
