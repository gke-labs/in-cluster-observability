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

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// queryTLSConfig builds the client TLS config for the query server's
// :9095/:9096 listeners (intra-ollie TLS, ADR-0029). The port-forward
// dials 127.0.0.1, so ServerName pins verification to the Service DNS
// SAN the serving cert actually carries. The trust anchor is ca.crt
// from the <service>-serving Secret — reading it needs Secret get
// permission in the install namespace; --insecure-tls is the explicit
// escape hatch for users without it.
func queryTLSConfig(ctx context.Context, g *globalOpts) (*tls.Config, error) {
	serverName := fmt.Sprintf("%s.%s.svc", g.service, g.namespace)
	if g.insecureTLS {
		//nolint:gosec // explicit operator opt-out via --insecure-tls
		return &tls.Config{InsecureSkipVerify: true, ServerName: serverName, MinVersion: tls.VersionTLS12}, nil
	}
	_, client, err := kubeClient(g)
	if err != nil {
		return nil, err
	}
	secretName := g.service + "-serving"
	sec, err := client.CoreV1().Secrets(g.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the ollie CA from Secret %s/%s: %w (pass --insecure-tls to skip server verification)", g.namespace, secretName, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(sec.Data["ca.crt"]) {
		return nil, fmt.Errorf("Secret %s/%s has no usable ca.crt — is the controller's CA manager running? (pass --insecure-tls to skip server verification)", g.namespace, secretName)
	}
	return &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12}, nil
}
