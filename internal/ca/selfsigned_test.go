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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelfSignedCert(t *testing.T) {
	cert, err := SelfSignedCert("ollie-query", []string{"ollie-query.ollie-system.svc", "localhost"})
	if err != nil {
		t.Fatalf("SelfSignedCert: %v", err)
	}
	if len(cert.Certificate) != 1 || cert.PrivateKey == nil {
		t.Fatal("incomplete certificate")
	}
}

// ServingTLSConfig serves the bootstrap self-signed cert while the
// files are missing and switches to the CA-issued keypair once they
// exist — the fresh-install posture every intra-ollie listener shares
// (ADR-0029).
func TestServingTLSConfigFallbackAndReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")

	conf, err := ServingTLSConfig(certFile, keyFile, "ollie-agent", []string{"ollie-agent.ollie-system.svc"}, nil)
	if err != nil {
		t.Fatalf("ServingTLSConfig: %v", err)
	}
	boot, err := conf.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate (bootstrap): %v", err)
	}
	if boot == nil || len(boot.Certificate) == 0 {
		t.Fatal("no bootstrap certificate served while files are missing")
	}

	// Drop a CA-issued keypair on disk; the next handshake must serve it.
	authority, err := NewCA(time.Now(), CADefaultLifetime)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := authority.IssueServingCert([]string{"ollie-agent.ollie-system.svc"}, time.Now(), ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("IssueServingCert: %v", err)
	}
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	issued, err := conf.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate (issued): %v", err)
	}
	if string(issued.Certificate[0]) == string(boot.Certificate[0]) {
		t.Fatal("still serving the bootstrap cert after the CA-issued keypair landed")
	}
	if err := VerifyServedBy(issued.Certificate[0], authority.CertPEM(), "ollie-agent.ollie-system.svc", time.Now()); err != nil {
		t.Fatalf("served leaf does not chain to the CA: %v", err)
	}
}
