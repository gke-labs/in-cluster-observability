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

package schema

import "testing"

func TestForwardableLabel(t *testing.T) {
	allowed := []string{
		// K8s identity, including dual-sided L4 attribution.
		LabelK8sPodName,
		LabelK8sNamespaceName,
		"k8s.src.namespace",
		"k8s.dst.name",
		// Service identity.
		LabelServiceName,
		LabelServiceNamespace,
		// Low-sensitivity HTTP + protocol dimensions.
		"http.request.method",
		"http.response.status_code",
		"http.route",
		"network.protocol.name",
		"tcp.connection.state",
	}
	for _, k := range allowed {
		if !ForwardableLabel(k) {
			t.Errorf("ForwardableLabel(%q) = false, want true", k)
		}
	}

	denied := []string{
		// The high-sensitivity keys #144 exists to exclude.
		"url.path",
		"url.full",
		"client.address",
		"server.address",
		"user_agent.original",
		// Prefix must not match bare family names or lookalikes.
		"k8s.",
		"k8s",
		"k8sfoo.pod.name",
		"servicex.name",
		// Underscore-normalized variants are not OTLP keys; the
		// forwarder filters pre-exposition, where keys are dotted.
		"k8s_pod_name",
		"",
	}
	for _, k := range denied {
		if ForwardableLabel(k) {
			t.Errorf("ForwardableLabel(%q) = true, want false", k)
		}
	}
}
