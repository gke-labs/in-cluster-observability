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

// Command probe is a tiny stdlib-only HTTP/TLS client the e2e harness
// ships into the cluster as a FROM-scratch image. It exists to exercise
// the ollie auth boundaries from a real pod IP: port-forward terminates
// on pod loopback, which every auth layer exempts by design (#145), so
// the negative-path tests (#195) cannot be written against a forwarded
// port. Two subcommands, each printing a single machine-parseable line
// prefixed "PROBE:" and always exiting 0 so the pod reaches Succeeded
// and its logs are retrievable regardless of the network outcome:
//
//	probe mtls URL
//	    Dial TCP to the URL's host:port (proving the listener is
//	    reachable), then issue an HTTPS GET presenting NO client
//	    certificate. Prints "PROBE: TCP_OK HTTPS_ERROR(<err>)" when the
//	    server rejects the certless client at the TLS layer, or
//	    "PROBE: TCP_OK HTTPS_STATUS <code> ..." when it answers at the
//	    HTTP layer. Used to prove the query front-proxy on :6443 admits
//	    only the aggregator (RequireAndVerifyClientCert against the
//	    requestheader CA) — the :6443 auth bypass closed in v0.5.1
//	    (#180). Server-cert verification is skipped on purpose so the
//	    only thing that can reject the request is the missing/untrusted
//	    CLIENT cert; a TCP_OK alongside the rejection proves it is
//	    TLS/auth-level, not a network or NetworkPolicy drop.
//
//	    An HTTPS GET (not a bare handshake) is required because TLS 1.3
//	    delivers the server's client-cert rejection as a post-handshake
//	    alert: tls.Dial returns success and the rejection only surfaces
//	    on the first read, i.e. when the HTTP round-trip runs.
//
//	probe get URL
//	    Plain HTTP GET with no Authorization header. Prints
//	    "PROBE: STATUS <code> ...". Used to prove the agent scrape
//	    surface on :9090 returns 401 to a tokenless caller that is
//	    network-permitted to reach it (#145).
//
//	probe tcp HOST:PORT
//	    Attempt a bare TCP connection with a short timeout. Prints
//	    "PROBE: TCP_OK" or "PROBE: TCP_FAIL(<err>)". Used to prove the
//	    agent scrape port :9090 is NetworkPolicy-DROPPED for a pod
//	    outside the permitted scraper namespace (#143): a dropped SYN
//	    never completes the handshake, so the dial times out. kindnet
//	    (KIND's default CNI) enforces NetworkPolicy via nftables on the
//	    node-image versions this suite runs.
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("PROBE: ERROR usage: probe <mtls|get|tcp> <target>")
		return
	}
	switch os.Args[1] {
	case "mtls":
		mtls(os.Args[2])
	case "get":
		get(os.Args[2])
	case "tcp":
		tcp(os.Args[2])
	default:
		fmt.Printf("PROBE: ERROR unknown subcommand %q\n", os.Args[1])
	}
}

// tcp attempts a bare TCP connection and reports reachability. A
// NetworkPolicy DROP shows up as a dial timeout (the SYN is silently
// discarded), distinguishing a network-layer block from an HTTP-layer
// rejection.
func tcp(addr string) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("PROBE: TCP_FAIL(%v)\n", err)
		return
	}
	_ = conn.Close()
	fmt.Println("PROBE: TCP_OK")
}

// mtls proves TCP reachability then issues a certless HTTPS GET, which
// an mTLS listener must reject (at the TLS layer on read, TLS 1.3).
func mtls(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		fmt.Printf("PROBE: ERROR bad url %q: %v\n", rawURL, err)
		return
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		fmt.Printf("PROBE: TCP_FAIL(%v)\n", err)
		return
	}
	_ = conn.Close()

	// InsecureSkipVerify skips SERVER-cert verification only; it does
	// not waive the server's requirement that the CLIENT present a cert.
	// Skipping it isolates the rejection to the missing client cert —
	// the server cert is a self-signed serving cert this bare pod has no
	// way to trust, and verifying it would fail for the wrong reason.
	c := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := c.Get(rawURL)
	if err != nil {
		fmt.Printf("PROBE: TCP_OK HTTPS_ERROR(%v)\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	fmt.Printf("PROBE: TCP_OK HTTPS_STATUS %d BODY %q\n", resp.StatusCode, strings.TrimSpace(string(body)))
}

// get issues a tokenless plain-HTTP GET and reports the status code.
func get(url string) {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Printf("PROBE: ERROR %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	fmt.Printf("PROBE: STATUS %d BODY %q\n", resp.StatusCode, strings.TrimSpace(string(body)))
}
