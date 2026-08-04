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

// Command tlsserver is a stdlib-only HTTPS echo server + client used by
// the TLS-decrypt e2e tests (#106). Its whole purpose is to put real
// encrypted L7 traffic through Go's crypto/tls on the pod network so
// OBI's Go-TLS uprobes have something to decrypt: the test then asserts
// that http.server.request.duration still appears for the server pod,
// proving OBI recovered the plaintext HTTP without a proxy or a flag.
//
// It is a scratch-image binary (like tests/e2e/probe), so it must stay
// on the standard library — no module deps. Two subcommands:
//
//	tlsserver serve                          # HTTPS :8443, self-signed
//	tlsserver client <url> <interval-ms>     # loop GETs, skip verify
//
// The server mints its own self-signed cert at startup so nothing has to
// be mounted; the client skips verification because the decrypt path,
// not the trust chain, is what is under test (chain-of-trust is covered
// by TestIntraTLSVerification).
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tlsserver serve | client <url> <interval-ms>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve()
	case "client":
		client()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

// serve listens HTTPS on :8443 with a freshly minted self-signed cert
// and answers every request with a small body. :8443 is in the agent's
// default --obi-instrument-ports seed, so no CR is needed to have OBI
// watch it.
func serve() {
	cert := selfSignedCert()
	srv := &http.Server{
		Addr: ":8443",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "ok\n")
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Println("tlsserver: serving HTTPS on :8443")
	// Certs come from TLSConfig, so the file args are empty.
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// client loops HTTPS GETs against the given URL forever, skipping
// certificate verification (the server is self-signed). The interval
// keeps a steady trickle of requests so the OBI series appears promptly
// without hammering the node.
func client() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: tlsserver client <url> <interval-ms>")
		os.Exit(2)
	}
	target := os.Args[2]
	intervalMS, err := strconv.Atoi(os.Args[3])
	if err != nil {
		log.Fatalf("tlsserver: bad interval %q: %v", os.Args[3], err)
	}
	c := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // e2e: decrypt path under test, not trust chain
		},
	}
	log.Printf("tlsserver: client looping GET %s every %dms", target, intervalMS)
	for {
		resp, err := c.Get(target)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		time.Sleep(time.Duration(intervalMS) * time.Millisecond)
	}
}

// selfSignedCert mints an in-memory ECDSA P-256 leaf. SANs are cosmetic
// here (the client skips verification) but are set so the cert is
// well-formed if anything ever inspects it.
func selfSignedCert() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("tlsserver: generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tls-go.default.svc"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", "tls-go.default.svc", "tls-go"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatalf("tlsserver: create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
