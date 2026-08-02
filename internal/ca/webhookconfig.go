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
	"context"
	"encoding/json"
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// WebhookStore abstracts the ValidatingWebhookConfiguration writes so
// the orchestration is unit-testable without an API server (mirrors
// APIServiceStore).
type WebhookStore interface {
	// Get reports whether every webhook entry already carries caPEM as
	// its caBundle AND failurePolicy Fail — the verified steady state.
	Get(ctx context.Context, caPEM []byte) (converged bool, err error)
	// Commit sets clientConfig.caBundle=caPEM and failurePolicy=Fail on
	// every webhook entry in one patch. The manifest ships the safe
	// bootstrap posture (empty caBundle + failurePolicy Ignore — CR
	// writes are never blocked by an unreachable webhook); the caller
	// guarantees every webhook endpoint already serves a caPEM-signed
	// leaf before enforcing Fail.
	Commit(ctx context.Context, caPEM []byte) error
}

type clientsetWebhookStore struct {
	cs   kubernetes.Interface
	name string
}

// NewClientsetWebhookStore drives the named
// ValidatingWebhookConfiguration through the typed clientset.
func NewClientsetWebhookStore(cs kubernetes.Interface, name string) WebhookStore {
	return &clientsetWebhookStore{cs: cs, name: name}
}

func (s *clientsetWebhookStore) Get(ctx context.Context, caPEM []byte) (bool, error) {
	cfg, err := s.cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, wh := range cfg.Webhooks {
		if string(wh.ClientConfig.CABundle) != string(caPEM) {
			return false, nil
		}
		if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionv1.Fail {
			return false, nil
		}
	}
	return true, nil
}

func (s *clientsetWebhookStore) Commit(ctx context.Context, caPEM []byte) error {
	cfg, err := s.cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	type whPatch struct {
		Name          string `json:"name"`
		FailurePolicy string `json:"failurePolicy"`
		ClientConfig  struct {
			CABundle []byte `json:"caBundle"`
		} `json:"clientConfig"`
	}
	patches := make([]whPatch, 0, len(cfg.Webhooks))
	for _, wh := range cfg.Webhooks {
		p := whPatch{Name: wh.Name, FailurePolicy: string(admissionv1.Fail)}
		p.ClientConfig.CABundle = caPEM
		patches = append(patches, p)
	}
	// Strategic merge keyed on webhook name: one atomic write covers
	// every entry's caBundle + failurePolicy together.
	body, err := json.Marshal(map[string]any{"webhooks": patches})
	if err != nil {
		return err
	}
	if _, err := s.cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Patch(
		ctx, s.name, types.StrategicMergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch ValidatingWebhookConfiguration %s: %w", s.name, err)
	}
	return nil
}
