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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log/slog"
	"math/big"
	"time"
)

// SelfSignedCert generates an in-memory ECDSA P-256 serving
// certificate for the given DNS names. It is the bootstrap fallback
// every TLS listener serves until the controller's CA manager has
// issued the real serving cert (fresh install) or when running outside
// a cluster (dev): traffic is encrypted from the first byte, and
// verifying clients converge once the CA-issued Secret is mounted.
func SelfSignedCert(commonName string, dnsNames []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}

// ServingTLSConfig builds the standard intra-ollie serving posture
// (ADR-0029): prefer the CA-issued keypair at certFile/keyFile
// (hot-reloaded on rotation via Reloader), fall back to a fresh
// self-signed certificate until those files exist. Callers that need
// client-cert auth (the custom-metrics front-proxy check) add
// ClientCAs/ClientAuth on the returned config; callers sharing it
// across listeners must Clone() per listener.
func ServingTLSConfig(certFile, keyFile, commonName string, dnsNames []string, logger *slog.Logger) (*tls.Config, error) {
	bootstrap, err := SelfSignedCert(commonName, dnsNames)
	if err != nil {
		return nil, fmt.Errorf("self-signed bootstrap cert: %w", err)
	}
	reloader := NewReloader(certFile, keyFile, logger)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if cert, rErr := reloader.GetCertificate(hello); rErr == nil {
				return cert, nil
			}
			return &bootstrap, nil
		},
	}, nil
}
