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
//	probe tcptls HOST:PORT
//	    Dial TCP (proving the listener is reachable), then attempt a TLS
//	    handshake presenting no client certificate. Prints
//	    "PROBE: TCP_OK TLS_FAIL(<err>)" when the server rejects the
//	    certless client. Used to prove the query front-proxy on :6443
//	    still requires a client cert (RequireAndVerifyClientCert) — the
//	    :6443 auth bypass closed in v0.5.1 (#180). Server-cert
//	    verification is skipped on purpose so the ONLY thing that can
//	    fail the handshake is the missing client cert; a TCP_OK next to
//	    TLS_FAIL proves the rejection is TLS-level auth, not a network or
//	    NetworkPolicy drop.
//
//	probe get URL
//	    Plain HTTP GET with no Authorization header. Prints
//	    "PROBE: STATUS <code> ...". Used to prove the agent scrape
//	    surface on :9090 returns 401 to a tokenless in-cluster caller
//	    (#145).
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("PROBE: ERROR usage: probe <tcptls|get> <target>")
		return
	}
	switch os.Args[1] {
	case "tcptls":
		tcptls(os.Args[2])
	case "get":
		get(os.Args[2])
	default:
		fmt.Printf("PROBE: ERROR unknown subcommand %q\n", os.Args[1])
	}
}

// tcptls proves reachability at TCP then attempts a certless TLS
// handshake, which an mTLS listener must reject.
func tcptls(addr string) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("PROBE: TCP_FAIL(%v)\n", err)
		return
	}
	_ = conn.Close()

	d := &net.Dialer{Timeout: 5 * time.Second}
	// InsecureSkipVerify skips SERVER-cert verification only; it does
	// not waive the server's requirement that the CLIENT present a cert.
	// Skipping it isolates the failure to the missing client cert — the
	// server cert is a self-signed serving cert this bare pod has no way
	// to trust, and verifying it would fail the handshake for the wrong
	// reason.
	tconn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		fmt.Printf("PROBE: TCP_OK TLS_FAIL(%v)\n", err)
		return
	}
	_ = tconn.Close()
	fmt.Println("PROBE: TCP_OK TLS_OK")
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
