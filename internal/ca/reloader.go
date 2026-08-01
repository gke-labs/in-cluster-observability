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
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Reloader serves a TLS keypair from files and hot-reloads it when the
// files change on disk. The query server plugs its GetCertificate into
// tls.Config so a rotated serving cert (a re-issued ollie-query-serving
// Secret, remounted by kubelet) is picked up without a restart.
//
// It never panics on a missing/unparseable file: GetCertificate returns
// an error, which the caller treats as "fall back to the self-signed
// bootstrap cert" (the fresh-install / dev path before the CA manager
// has issued the real cert).
type Reloader struct {
	certFile, keyFile string
	logger            *slog.Logger

	mu     sync.RWMutex
	cached *tls.Certificate
	stamp  time.Time // newest mtime observed across the two files
}

// NewReloader returns a Reloader for the given cert/key file paths.
func NewReloader(certFile, keyFile string, logger *slog.Logger) *Reloader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reloader{certFile: certFile, keyFile: keyFile, logger: logger}
}

// GetCertificate satisfies tls.Config.GetCertificate. It reloads when
// either file's mtime advances past the last load, and otherwise serves
// the cached keypair.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	newest, err := r.newestMod()
	if err != nil {
		return nil, err
	}

	r.mu.RLock()
	cached, stamp := r.cached, r.stamp
	r.mu.RUnlock()
	if cached != nil && !newest.After(stamp) {
		return cached, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check under the write lock in case another goroutine reloaded.
	if r.cached != nil && !newest.After(r.stamp) {
		return r.cached, nil
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("load serving keypair: %w", err)
	}
	r.cached = &cert
	r.stamp = newest
	r.logger.Info("loaded serving certificate from disk", "cert", r.certFile)
	return r.cached, nil
}

func (r *Reloader) newestMod() (time.Time, error) {
	var newest time.Time
	for _, f := range []string{r.certFile, r.keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			return time.Time{}, err
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest, nil
}
