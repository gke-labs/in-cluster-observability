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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeAPIService is an in-memory APIServiceStore. It mirrors the real
// aggregation-API invariant: a non-empty caBundle may not coexist with
// insecureSkipTLSVerify=true, so the only mutation is the atomic Commit.
type fakeAPIService struct {
	caBundle    []byte
	insecure    bool
	commitCalls int
}

func (f *fakeAPIService) Get(context.Context) ([]byte, bool, error) {
	return f.caBundle, f.insecure, nil
}
func (f *fakeAPIService) Commit(_ context.Context, ca []byte) error {
	f.caBundle = ca
	f.insecure = false
	f.commitCalls++
	return nil
}

const (
	testNS      = "ollie-system"
	testCASec   = "ollie-ca"
	testServSec = "ollie-query-serving"
	testSvc     = "ollie-query"
)

var testDNS = []string{"ollie-query.ollie-system.svc", "ollie-query.ollie-system.svc.cluster.local", "localhost"}

func newManager(t *testing.T, objs ...corev1.Secret) (*Manager, *fake.Clientset, *fakeAPIService) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	for i := range objs {
		if _, err := cs.CoreV1().Secrets(testNS).Create(context.Background(), &objs[i], metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed secret: %v", err)
		}
	}
	api := &fakeAPIService{insecure: true}
	m := &Manager{
		Clientset:       cs,
		APISvc:          api,
		Namespace:       testNS,
		CASecret:        testCASec,
		ServingSecret:   testServSec,
		ServingDNSNames: testDNS,
		QueryService:    testSvc,
		TLSPort:         6443,
		now:             time.Now,
	}
	return m, cs, api
}

func getSecret(t *testing.T, cs *fake.Clientset, name string) *corev1.Secret {
	t.Helper()
	s, err := cs.CoreV1().Secrets(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret %s: %v", name, err)
	}
	return s
}

func TestEnsureCACreatesAndReloads(t *testing.T) {
	m, cs, _ := newManager(t)
	ctx := context.Background()

	c1, err := m.ensureCA(ctx)
	if err != nil {
		t.Fatalf("ensureCA (create): %v", err)
	}
	sec := getSecret(t, cs, testCASec)
	if sec.Type != corev1.SecretTypeTLS {
		t.Errorf("CA secret type = %s, want kubernetes.io/tls", sec.Type)
	}
	// Second call must reload the SAME CA, not mint a new one.
	c2, err := m.ensureCA(ctx)
	if err != nil {
		t.Fatalf("ensureCA (reload): %v", err)
	}
	if string(c1.CertPEM()) != string(c2.CertPEM()) {
		t.Error("ensureCA minted a new CA instead of reloading the stored one")
	}
}

func TestEnsureCARefusesCorruptSecret(t *testing.T) {
	bad := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCASec, Namespace: testNS},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("garbage"), corev1.TLSPrivateKeyKey: []byte("garbage")},
	}
	m, _, _ := newManager(t, bad)
	if _, err := m.ensureCA(context.Background()); err == nil {
		t.Fatal("ensureCA overwrote/accepted a corrupt CA secret")
	}
}

func TestEnsureServingCertLifecycle(t *testing.T) {
	m, cs, _ := newManager(t)
	ctx := context.Background()
	authority, _ := m.ensureCA(ctx)

	// Create.
	if err := m.ensureServingCert(ctx, authority); err != nil {
		t.Fatalf("ensureServingCert (create): %v", err)
	}
	sec := getSecret(t, cs, testServSec)
	first := sec.Data[corev1.TLSCertKey]
	if len(first) == 0 {
		t.Fatal("serving secret missing tls.crt")
	}
	if !ServingCertMatches(first, authority.CertPEM(), testDNS, time.Now()) {
		t.Fatal("issued serving cert does not match CA/SANs")
	}
	if len(sec.Data["ca.crt"]) == 0 {
		t.Error("serving secret should also carry ca.crt for #197 consumers")
	}

	// Idempotent: healthy cert is not re-issued.
	if err := m.ensureServingCert(ctx, authority); err != nil {
		t.Fatalf("ensureServingCert (noop): %v", err)
	}
	if string(getSecret(t, cs, testServSec).Data[corev1.TLSCertKey]) != string(first) {
		t.Error("healthy serving cert was needlessly re-issued")
	}

	// SAN drift forces re-issue.
	m.ServingDNSNames = append(testDNS, "extra.svc")
	if err := m.ensureServingCert(ctx, authority); err != nil {
		t.Fatalf("ensureServingCert (drift): %v", err)
	}
	if string(getSecret(t, cs, testServSec).Data[corev1.TLSCertKey]) == string(first) {
		t.Error("serving cert not re-issued after SAN change")
	}
}

func TestEnsureServingCertReissuesNearExpiry(t *testing.T) {
	m, cs, _ := newManager(t)
	ctx := context.Background()
	authority, _ := m.ensureCA(ctx)
	m.ServingLifetime = 10 * time.Minute
	m.RenewBefore = 9 * time.Minute // renew window covers most of the life

	if err := m.ensureServingCert(ctx, authority); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := getSecret(t, cs, testServSec).Data[corev1.TLSCertKey]

	// Advance clock to inside the renewal window.
	m.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := m.ensureServingCert(ctx, authority); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if string(getSecret(t, cs, testServSec).Data[corev1.TLSCertKey]) == string(first) {
		t.Error("serving cert not renewed inside RenewBefore window")
	}
}

func TestReconcileAPIServiceGate(t *testing.T) {
	m, cs, api := newManager(t)
	ctx := context.Background()
	authority, _ := m.ensureCA(ctx)

	// Leaf served by the CA, and a leaf from a foreign CA.
	goodPEM, _, _ := authority.IssueServingCert(testDNS, time.Now(), ServingDefaultLifetime)
	goodDER, _ := decodePEM(goodPEM, "CERTIFICATE")
	foreign := mustCA(t)
	badPEM, _, _ := foreign.IssueServingCert(testDNS, time.Now(), ServingDefaultLifetime)
	badDER, _ := decodePEM(badPEM, "CERTIFICATE")

	// No endpoints yet: no commit — bootstrap posture untouched.
	if err := m.reconcileAPIService(ctx, authority); err != nil {
		t.Fatalf("reconcileAPIService (no endpoints): %v", err)
	}
	if !api.insecure || len(api.caBundle) != 0 {
		t.Fatal("committed with zero ready endpoints (must keep skip-verify on, empty caBundle)")
	}

	// Two endpoints, one still serving a foreign (old self-signed) cert.
	seedEndpoints(t, cs, "10.0.0.1", "10.0.0.2")
	served := map[string][]byte{
		"10.0.0.1:6443": goodDER,
		"10.0.0.2:6443": badDER,
	}
	m.probeLeaf = func(_ context.Context, addr string) ([]byte, error) { return served[addr], nil }
	if err := m.reconcileAPIService(ctx, authority); err != nil {
		t.Fatalf("reconcileAPIService (mixed): %v", err)
	}
	if !api.insecure || len(api.caBundle) != 0 {
		t.Fatal("committed while an endpoint still served a non-CA cert (HPA takedown risk)")
	}

	// Both now serve the CA cert: commit proceeds atomically.
	served["10.0.0.2:6443"] = goodDER
	if err := m.reconcileAPIService(ctx, authority); err != nil {
		t.Fatalf("reconcileAPIService (all good): %v", err)
	}
	if api.insecure {
		t.Fatal("did not clear insecureSkipTLSVerify once all endpoints served the CA cert")
	}
	if string(api.caBundle) != string(authority.CertPEM()) {
		t.Fatal("caBundle not set to the CA cert on commit")
	}
	if api.commitCalls != 1 {
		t.Errorf("commit called %d times, want 1", api.commitCalls)
	}

	// Already committed and current: no-op (and no probing needed).
	m.probeLeaf = func(_ context.Context, _ string) ([]byte, error) { t.Fatal("probed after commit"); return nil, nil }
	if err := m.reconcileAPIService(ctx, authority); err != nil {
		t.Fatalf("reconcileAPIService (post-commit): %v", err)
	}
	if api.commitCalls != 1 {
		t.Errorf("commit called %d times after steady state, want 1", api.commitCalls)
	}
}

func TestReconcileEndToEnd(t *testing.T) {
	m, cs, api := newManager(t)
	ctx := context.Background()

	// Endpoints exist and will serve whatever the manager issued.
	seedEndpoints(t, cs, "10.0.0.5")
	m.probeLeaf = func(_ context.Context, _ string) ([]byte, error) {
		// Mirror what a query pod would serve: the current serving cert.
		s := getSecret(t, cs, testServSec)
		der, _ := decodePEM(s.Data[corev1.TLSCertKey], "CERTIFICATE")
		return der, nil
	}

	if err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// CA + serving secrets exist, caBundle set, flipped.
	getSecret(t, cs, testCASec)
	getSecret(t, cs, testServSec)
	if len(api.caBundle) == 0 {
		t.Error("caBundle not populated")
	}
	if api.insecure {
		t.Error("insecureSkipTLSVerify not dropped after healthy reconcile")
	}
}

func seedEndpoints(t *testing.T, cs *fake.Clientset, ips ...string) {
	t.Helper()
	addrs := make([]corev1.EndpointAddress, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, corev1.EndpointAddress{IP: ip})
	}
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: testSvc, Namespace: testNS},
		Subsets:    []corev1.EndpointSubset{{Addresses: addrs}},
	}
	if _, err := cs.CoreV1().Endpoints(testNS).Create(context.Background(), ep, metav1.CreateOptions{}); err != nil {
		// Update if it already exists.
		if _, uErr := cs.CoreV1().Endpoints(testNS).Update(context.Background(), ep, metav1.UpdateOptions{}); uErr != nil {
			t.Fatalf("seed endpoints: %v", err)
		}
	}
}
