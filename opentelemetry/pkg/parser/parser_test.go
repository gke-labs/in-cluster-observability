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

package parser

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected *Query
	}{
		{
			name:  "empty query",
			input: "",
			expected: &Query{
				BodyContains: []string{},
				Attributes:   map[string]string{},
			},
		},
		{
			name:  "simple text terms",
			input: "connection refused timeout",
			expected: &Query{
				BodyContains: []string{"connection", "refused", "timeout"},
				Attributes:   map[string]string{},
			},
		},
		{
			name:  "quoted text terms",
			input: `"connection refused" timeout`,
			expected: &Query{
				BodyContains: []string{"connection refused", "timeout"},
				Attributes:   map[string]string{},
			},
		},
		{
			name:  "well known aliases",
			input: "namespace=kube-system pod=coredns-* container=dns service=my-service severity=error",
			expected: &Query{
				BodyContains: []string{},
				Attributes: map[string]string{
					"k8s.namespace.name": "kube-system",
					"k8s.pod.name":       "coredns-*",
					"k8s.container.name": "dns",
					"service.name":       "my-service",
					"SeverityText":       "error",
				},
			},
		},
		{
			name:  "unknown keys",
			input: "custom.attr=val1 custom_other=val2",
			expected: &Query{
				BodyContains: []string{},
				Attributes: map[string]string{
					"custom.attr":  "val1",
					"custom_other": "val2",
				},
			},
		},
		{
			name:  "quoted key value",
			input: `app="my app" namespace="prod-env"`,
			expected: &Query{
				BodyContains: []string{},
				Attributes: map[string]string{
					"app":                "my app",
					"k8s.namespace.name": "prod-env",
				},
			},
		},
		{
			name:  "complex mix",
			input: `namespace=kube-system pod=coredns-* severity=error "connection refused"`,
			expected: &Query{
				BodyContains: []string{"connection refused"},
				Attributes: map[string]string{
					"k8s.namespace.name": "kube-system",
					"k8s.pod.name":       "coredns-*",
					"SeverityText":       "error",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := Parse(tc.input)
			if !reflect.DeepEqual(actual.BodyContains, tc.expected.BodyContains) {
				t.Errorf("BodyContains mismatch: got %v, want %v", actual.BodyContains, tc.expected.BodyContains)
			}
			if !reflect.DeepEqual(actual.Attributes, tc.expected.Attributes) {
				t.Errorf("Attributes mismatch: got %v, want %v", actual.Attributes, tc.expected.Attributes)
			}
		})
	}
}
