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
	"encoding/base64"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// apiServiceGVR is the aggregation-layer APIService resource. We reach it
// through the dynamic client rather than importing kube-aggregator's
// typed clientset, which is not otherwise a dependency.
var apiServiceGVR = schema.GroupVersionResource{
	Group:    "apiregistration.k8s.io",
	Version:  "v1",
	Resource: "apiservices",
}

// dynamicAPIServiceStore implements APIServiceStore over the dynamic
// client. APIServices are cluster-scoped.
type dynamicAPIServiceStore struct {
	client dynamic.Interface
	name   string
}

// NewDynamicAPIServiceStore wires an APIServiceStore for the named
// APIService (e.g. v1beta1.custom.metrics.k8s.io).
func NewDynamicAPIServiceStore(client dynamic.Interface, name string) APIServiceStore {
	return &dynamicAPIServiceStore{client: client, name: name}
}

func (s *dynamicAPIServiceStore) Get(ctx context.Context) (caBundle []byte, insecure bool, err error) {
	obj, err := s.client.Resource(apiServiceGVR).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("get APIService %s: %w", s.name, err)
	}
	// spec.caBundle is []byte, JSON-encoded as a base64 string.
	if raw, found, _ := unstructuredString(obj.Object, "spec", "caBundle"); found && raw != "" {
		decoded, dErr := base64.StdEncoding.DecodeString(raw)
		if dErr != nil {
			return nil, false, fmt.Errorf("decode caBundle: %w", dErr)
		}
		caBundle = decoded
	}
	insecure, _, _ = unstructuredBool(obj.Object, "spec", "insecureSkipTLSVerify")
	return caBundle, insecure, nil
}

func (s *dynamicAPIServiceStore) Commit(ctx context.Context, caPEM []byte) error {
	// caBundle and insecureSkipTLSVerify:false must land in the same patch:
	// the API server rejects a non-empty caBundle while skip-verify is true,
	// so a two-step (set bundle, then clear flag) is not representable.
	patch := map[string]any{
		"spec": map[string]any{
			"caBundle":              base64.StdEncoding.EncodeToString(caPEM),
			"insecureSkipTLSVerify": false,
		},
	}
	return s.patch(ctx, patch)
}

func (s *dynamicAPIServiceStore) patch(ctx context.Context, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = s.client.Resource(apiServiceGVR).Patch(ctx, s.name, types.MergePatchType, data, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch APIService %s: %w", s.name, err)
	}
	return nil
}

func unstructuredString(obj map[string]any, path ...string) (string, bool, error) {
	v, found, err := nestedField(obj, path...)
	if err != nil || !found {
		return "", found, err
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("%v is not a string", path)
	}
	return s, true, nil
}

func unstructuredBool(obj map[string]any, path ...string) (bool, bool, error) {
	v, found, err := nestedField(obj, path...)
	if err != nil || !found {
		return false, found, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, true, fmt.Errorf("%v is not a bool", path)
	}
	return b, true, nil
}

func nestedField(obj map[string]any, path ...string) (any, bool, error) {
	cur := any(obj)
	for i, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("%v is not a map", path[:i])
		}
		cur, ok = m[p]
		if !ok {
			return nil, false, nil
		}
	}
	return cur, true, nil
}
