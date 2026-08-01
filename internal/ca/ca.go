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

// Package ca is the self-managed certificate authority for ollie's
// in-cluster TLS (ADR-0028). It owns the pure crypto: mint a long-lived
// CA, issue short-lived serving certificates signed by it, and verify a
// served leaf chains back to the CA. All state is PEM bytes so callers
// can persist it in Secrets without importing crypto/x509.
//
// There is no dependency on cert-manager: the CA private key is minted
// in-process and stored in a Kubernetes Secret that only the controller
// reads (see manager.go). The design deliberately keeps this file
// cluster-free so the crypto is unit-testable without an API server —
// which matters because the integration path can only be exercised in
// CI.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// CADefaultLifetime is how long a freshly minted CA is valid. The CA is
// the root of trust distributed in APIService.spec.caBundle; rotating it
// is a two-phase, cluster-wide operation (ADR-0028 §rotation), so it is
// deliberately long-lived.
const CADefaultLifetime = 5 * 365 * 24 * time.Hour

// ServingDefaultLifetime is how long an issued serving certificate is
// valid. Serving certs are cheap to re-issue (the CA never changes), so
// they are short-lived and renewed well before expiry by the manager.
const ServingDefaultLifetime = 90 * 24 * time.Hour

// CA is a certificate authority: an ECDSA P-256 key plus its self-signed
// CA certificate. Construct one with NewCA (fresh) or Parse (from stored
// PEM).
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	keyPEM  []byte
}

// NewCA mints a fresh self-signed CA valid for the given lifetime. The
// key never leaves the process except as KeyPEM, which the caller stores
// in a Secret readable only by the controller.
func NewCA(now time.Time, lifetime time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "ollie-ca",
			Organization: []string{"ollie"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	return newCAFrom(der, key)
}

// Parse reconstructs a CA from stored PEM (the ollie-ca Secret's
// tls.crt/tls.key). It fails if the certificate is not a CA or the key
// does not match the certificate.
func Parse(certPEM, keyPEM []byte) (*CA, error) {
	certDER, err := decodePEM(certPEM, "CERTIFICATE")
	if err != nil {
		return nil, fmt.Errorf("CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("stored certificate is not a CA")
	}
	keyDER, err := decodePEM(keyPEM, "EC PRIVATE KEY")
	if err != nil {
		return nil, fmt.Errorf("CA key: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.X.Cmp(key.PublicKey.X) != 0 || pub.Y.Cmp(key.PublicKey.Y) != 0 {
		return nil, fmt.Errorf("CA key does not match CA certificate")
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, keyPEM: keyPEM}, nil
}

func newCAFrom(der []byte, key *ecdsa.PrivateKey) (*CA, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse minted CA cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &CA{cert: cert, key: key, certPEM: certPEM, keyPEM: keyPEM}, nil
}

// CertPEM returns the PEM-encoded CA certificate. This is what is
// distributed as APIService.spec.caBundle and as the ca.crt consumers
// trust; it never contains the private key.
func (c *CA) CertPEM() []byte { return c.certPEM }

// KeyPEM returns the PEM-encoded CA private key. Store only in the
// controller-readable ollie-ca Secret; never distribute.
func (c *CA) KeyPEM() []byte { return c.keyPEM }

// NotAfter is the CA certificate's expiry.
func (c *CA) NotAfter() time.Time { return c.cert.NotAfter }

// IssueServingCert mints a serving certificate for the given DNS SANs,
// signed by the CA, with the ServerAuth EKU. Returns the leaf cert and
// its key as PEM (the ollie-query-serving Secret's tls.crt/tls.key). The
// leaf is self-contained: the aggregator supplies the root via caBundle,
// so no intermediates are bundled.
func (c *CA) IssueServingCert(dnsNames []string, now time.Time, lifetime time.Duration) (certPEM, keyPEM []byte, err error) {
	if len(dnsNames) == 0 {
		return nil, nil, fmt.Errorf("serving cert needs at least one DNS name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate serving key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign serving cert: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal serving key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// VerifyServedBy reports whether leafDER (a certificate as presented on
// the wire) was issued by caPEM. It is the flip gate's check: the
// controller refuses to drop insecureSkipTLSVerify until every ollie-
// query endpoint presents a leaf that chains to the current CA. dnsName,
// if non-empty, must also match a SAN.
func VerifyServedBy(leafDER, caPEM []byte, dnsName string, now time.Time) error {
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return fmt.Errorf("parse served leaf: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("caPEM contains no usable certificate")
	}
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		DNSName:     dnsName,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("served leaf not issued by current CA: %w", err)
	}
	return nil
}

// ServingCertExpiry parses a stored serving-cert PEM and returns its
// NotAfter, so the manager can decide whether to renew.
func ServingCertExpiry(certPEM []byte) (time.Time, error) {
	der, err := decodePEM(certPEM, "CERTIFICATE")
	if err != nil {
		return time.Time{}, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse serving cert: %w", err)
	}
	return cert.NotAfter, nil
}

// ServingCertMatches reports whether a stored serving cert was issued by
// the given CA and covers exactly the wanted DNS names. A false result
// means the manager must re-issue (CA rotated, or SANs changed).
func ServingCertMatches(certPEM, caPEM []byte, dnsNames []string, now time.Time) bool {
	der, err := decodePEM(certPEM, "CERTIFICATE")
	if err != nil {
		return false
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return false
	}
	if VerifyServedBy(der, caPEM, "", now) != nil {
		return false
	}
	have := map[string]bool{}
	for _, n := range leaf.DNSNames {
		have[n] = true
	}
	for _, n := range dnsNames {
		if !have[n] {
			return false
		}
	}
	return len(leaf.DNSNames) == len(dnsNames)
}

func randSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return serial, nil
}

func decodePEM(data []byte, want string) ([]byte, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if block.Type != want {
		return nil, fmt.Errorf("expected PEM type %q, got %q", want, block.Type)
	}
	return block.Bytes, nil
}
