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

package scrapeauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFake builds a fake clientset where token "good-token"
// authenticates as user scraper@test and SAR allows only that user.
// The two counters report how many API round-trips happened.
func newFake(t *testing.T) (client *fake.Clientset, tokenReviews, sars *atomic.Int32) {
	t.Helper()
	client = fake.NewClientset()
	tokenReviews, sars = &atomic.Int32{}, &atomic.Int32{}

	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		tokenReviews.Add(1)
		tr := action.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		if tr.Spec.Token == "error-token" {
			return true, nil, errors.New("apiserver unavailable")
		}
		out := tr.DeepCopy()
		if tr.Spec.Token == "good-token" || tr.Spec.Token == "unauthorized-token" {
			out.Status.Authenticated = true
			out.Status.User = authnv1.UserInfo{Username: "user-" + tr.Spec.Token}
		}
		return true, out, nil
	})
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sars.Add(1)
		sar := action.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		out := sar.DeepCopy()
		out.Status.Allowed = sar.Spec.User == "user-good-token" &&
			sar.Spec.NonResourceAttributes != nil &&
			sar.Spec.NonResourceAttributes.Verb == "get"
		return true, out, nil
	})
	return client, tokenReviews, sars
}

func serve(m *Middleware, token, remoteAddr string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	m.Wrap(next).ServeHTTP(rec, req)
	return rec
}

func TestWrapDecisions(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"unauthenticated token", "bad-token", http.StatusUnauthorized},
		{"authenticated but unauthorized", "unauthorized-token", http.StatusForbidden},
		{"authorized", "good-token", http.StatusOK},
		{"apiserver error fails closed", "error-token", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _ := newFake(t)
			m := New(Config{Client: client})
			if got := serve(m, tc.token, "").Code; got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDecisionCaching(t *testing.T) {
	client, tokenReviews, sars := newFake(t)
	m := New(Config{Client: client})

	for i := 0; i < 5; i++ {
		if got := serve(m, "good-token", "").Code; got != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, got)
		}
	}
	if tokenReviews.Load() != 1 || sars.Load() != 1 {
		t.Errorf("API calls = (%d TokenReview, %d SAR), want (1, 1) — decisions should cache",
			tokenReviews.Load(), sars.Load())
	}

	// Denials cache too (with their own TTL).
	for i := 0; i < 3; i++ {
		serve(m, "unauthorized-token", "")
	}
	if tokenReviews.Load() != 2 {
		t.Errorf("TokenReviews after cached denials = %d, want 2", tokenReviews.Load())
	}
}

func TestAPIErrorNotCached(t *testing.T) {
	client, tokenReviews, _ := newFake(t)
	m := New(Config{Client: client})
	serve(m, "error-token", "")
	serve(m, "error-token", "")
	if tokenReviews.Load() != 2 {
		t.Errorf("TokenReviews = %d, want 2 — 500s must not cache", tokenReviews.Load())
	}
}

func TestLoopbackExemption(t *testing.T) {
	client, tokenReviews, _ := newFake(t)

	m := New(Config{Client: client, ExemptLoopback: true})
	if got := serve(m, "", "127.0.0.1:54321").Code; got != http.StatusOK {
		t.Errorf("loopback with exemption: status = %d, want 200", got)
	}
	if got := serve(m, "", "[::1]:54321").Code; got != http.StatusOK {
		t.Errorf("IPv6 loopback with exemption: status = %d, want 200", got)
	}
	if tokenReviews.Load() != 0 {
		t.Errorf("loopback exemption performed %d TokenReviews, want 0", tokenReviews.Load())
	}
	if got := serve(m, "", "10.0.0.7:54321").Code; got != http.StatusUnauthorized {
		t.Errorf("non-loopback without token: status = %d, want 401", got)
	}

	// Exemption off: loopback still needs a token.
	m2 := New(Config{Client: client})
	if got := serve(m2, "", "127.0.0.1:54321").Code; got != http.StatusUnauthorized {
		t.Errorf("loopback without exemption: status = %d, want 401", got)
	}
}

func TestAudiencesForwarded(t *testing.T) {
	client, _, _ := newFake(t)
	var gotAudiences []string
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		tr := action.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		gotAudiences = tr.Spec.Audiences
		out := tr.DeepCopy()
		out.Status.Authenticated = true
		out.Status.User = authnv1.UserInfo{Username: "user-good-token"}
		return true, out, nil
	})

	m := New(Config{Client: client, Audiences: []string{"ollie"}})
	if got := serve(m, "good-token", "").Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if len(gotAudiences) != 1 || gotAudiences[0] != "ollie" {
		t.Errorf("TokenReview audiences = %v, want [ollie]", gotAudiences)
	}
}
