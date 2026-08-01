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
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKeypair(t *testing.T, dir string, authority *CA, cn string) {
	t.Helper()
	certPEM, keyPEM, err := authority.IssueServingCert([]string{cn}, time.Now(), ServingDefaultLifetime)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func leafCN(t *testing.T, r *Reloader) string {
	t.Helper()
	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf.Subject.CommonName
}

func TestReloaderHotSwap(t *testing.T) {
	dir := t.TempDir()
	authority := mustCA(t)
	writeKeypair(t, dir, authority, "first.svc")

	r := NewReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil)
	if got := leafCN(t, r); got != "first.svc" {
		t.Fatalf("initial CN = %q, want first.svc", got)
	}

	// Rewrite with a new cert and advance mtime; the reloader must swap.
	writeKeypair(t, dir, authority, "second.svc")
	future := time.Now().Add(time.Hour)
	for _, f := range []string{"tls.crt", "tls.key"} {
		if err := os.Chtimes(filepath.Join(dir, f), future, future); err != nil {
			t.Fatal(err)
		}
	}
	if got := leafCN(t, r); got != "second.svc" {
		t.Fatalf("after rotation CN = %q, want second.svc", got)
	}
}

func TestReloaderMissingFilesErrors(t *testing.T) {
	dir := t.TempDir()
	r := NewReloader(filepath.Join(dir, "nope.crt"), filepath.Join(dir, "nope.key"), nil)
	// Missing files must surface an error so the caller can fall back to
	// the self-signed bootstrap cert rather than serve a broken handshake.
	if _, err := r.GetCertificate(nil); err == nil {
		t.Fatal("expected error for missing cert files")
	}
}

func TestReloaderCachesWithoutChange(t *testing.T) {
	dir := t.TempDir()
	authority := mustCA(t)
	writeKeypair(t, dir, authority, "stable.svc")
	r := NewReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil)

	c1, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	// No file change => same cached pointer, no re-parse.
	if c1 != c2 {
		t.Error("reloader re-parsed an unchanged cert instead of serving cache")
	}
}
