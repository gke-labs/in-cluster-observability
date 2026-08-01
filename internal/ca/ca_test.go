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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func mustCA(t *testing.T) *CA {
	t.Helper()
	c, err := NewCA(time.Now(), CADefaultLifetime)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	return c
}

func TestNewCAIsCA(t *testing.T) {
	c := mustCA(t)
	der, err := decodePEM(c.CertPEM(), "CERTIFICATE")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cert.IsCA {
		t.Error("minted CA cert is not marked IsCA")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA missing KeyUsageCertSign")
	}
	if !cert.MaxPathLenZero {
		t.Error("CA should have path length 0 (no intermediates)")
	}
}

func TestParseRoundTrip(t *testing.T) {
	c := mustCA(t)
	got, err := Parse(c.CertPEM(), c.KeyPEM())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if string(got.CertPEM()) != string(c.CertPEM()) {
		t.Error("cert PEM changed across round trip")
	}
	if string(got.KeyPEM()) != string(c.KeyPEM()) {
		t.Error("key PEM changed across round trip")
	}
}

func TestParseRejectsMismatchedKey(t *testing.T) {
	c1 := mustCA(t)
	c2 := mustCA(t)
	if _, err := Parse(c1.CertPEM(), c2.KeyPEM()); err == nil {
		t.Fatal("Parse accepted a key that does not match the cert")
	}
}

func TestParseRejectsNonCA(t *testing.T) {
	c := mustCA(t)
	// A serving cert is not a CA; storing it in the CA slot must fail.
	certPEM, keyPEM, err := c.IssueServingCert([]string{"x.svc"}, time.Now(), ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := Parse(certPEM, keyPEM); err == nil {
		t.Fatal("Parse accepted a non-CA certificate")
	}
}

func TestIssueServingCertChainsToCA(t *testing.T) {
	c := mustCA(t)
	names := []string{"ollie-query.ollie-system.svc", "ollie-query.ollie-system.svc.cluster.local", "localhost"}
	certPEM, keyPEM, err := c.IssueServingCert(names, time.Now(), ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("IssueServingCert: %v", err)
	}
	// It must load as a usable TLS keypair.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("issued keypair invalid: %v", err)
	}
	der, err := decodePEM(certPEM, "CERTIFICATE")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Verifies against the CA for each SAN.
	for _, n := range names {
		if err := VerifyServedBy(der, c.CertPEM(), n, time.Now()); err != nil {
			t.Errorf("SAN %q did not verify: %v", n, err)
		}
	}
	// A name that is not a SAN must fail hostname verification.
	if err := VerifyServedBy(der, c.CertPEM(), "evil.example.com", time.Now()); err == nil {
		t.Error("verification accepted a non-SAN hostname")
	}
}

func TestVerifyServedByRejectsForeignCA(t *testing.T) {
	issuer := mustCA(t)
	other := mustCA(t)
	certPEM, _, err := issuer.IssueServingCert([]string{"x.svc"}, time.Now(), ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	der, _ := decodePEM(certPEM, "CERTIFICATE")
	// Presented leaf was signed by issuer, but we check against other:
	// this is exactly the flip-gate failure mode (stale serving cert
	// during a CA rotation or upgrade window).
	if err := VerifyServedBy(der, other.CertPEM(), "", time.Now()); err == nil {
		t.Fatal("flip gate would have accepted a cert not signed by the current CA")
	}
}

func TestVerifyServedByRejectsExpired(t *testing.T) {
	c := mustCA(t)
	past := time.Now().Add(-48 * time.Hour)
	certPEM, _, err := c.IssueServingCert([]string{"x.svc"}, past, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	der, _ := decodePEM(certPEM, "CERTIFICATE")
	if err := VerifyServedBy(der, c.CertPEM(), "", time.Now()); err == nil {
		t.Fatal("verification accepted an expired serving cert")
	}
}

func TestServingCertMatches(t *testing.T) {
	c := mustCA(t)
	names := []string{"a.svc", "b.svc"}
	certPEM, _, err := c.IssueServingCert(names, time.Now(), ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	now := time.Now()
	if !ServingCertMatches(certPEM, c.CertPEM(), names, now) {
		t.Error("matching cert reported as non-matching")
	}
	// Same names, different order — still a match.
	if !ServingCertMatches(certPEM, c.CertPEM(), []string{"b.svc", "a.svc"}, now) {
		t.Error("SAN order should not matter")
	}
	// Extra requested name — needs re-issue.
	if ServingCertMatches(certPEM, c.CertPEM(), []string{"a.svc", "b.svc", "c.svc"}, now) {
		t.Error("cert missing a requested SAN should not match")
	}
	// Fewer requested names — the cert over-covers, needs re-issue.
	if ServingCertMatches(certPEM, c.CertPEM(), []string{"a.svc"}, now) {
		t.Error("cert with extra SAN should not match a smaller request")
	}
	// Different CA — needs re-issue.
	other := mustCA(t)
	if ServingCertMatches(certPEM, other.CertPEM(), names, now) {
		t.Error("cert not signed by the given CA should not match")
	}
}

func TestServingCertExpiry(t *testing.T) {
	c := mustCA(t)
	now := time.Now().Truncate(time.Second)
	certPEM, _, err := c.IssueServingCert([]string{"x.svc"}, now, ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	exp, err := ServingCertExpiry(certPEM)
	if err != nil {
		t.Fatalf("expiry: %v", err)
	}
	want := now.Add(ServingDefaultLifetime)
	if exp.Sub(want).Abs() > time.Minute {
		t.Errorf("expiry = %v, want ~%v", exp, want)
	}
}

func TestDecodePEMTypeMismatch(t *testing.T) {
	blk := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})
	if _, err := decodePEM(blk, "EC PRIVATE KEY"); err == nil {
		t.Error("decodePEM accepted the wrong block type")
	}
}
