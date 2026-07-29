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
	"fmt"
	"net/http"
	"os"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// forward port-forwards to a Ready query-server pod and returns the
// local port plus a stop function.
func forward(ctx context.Context, g *globalOpts, remotePort int) (int, func(), error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if g.kubeconfig != "" {
		rules.ExplicitPath = g.kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: g.kubectx},
	).ClientConfig()
	if err != nil {
		return 0, nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return 0, nil, err
	}

	pod, err := pickReadyPod(ctx, client, g.namespace)
	if err != nil {
		return 0, nil, err
	}

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return 0, nil, err
	}
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(g.namespace).Name(pod).SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", remotePort)}, stopCh, readyCh, nil, os.Stderr)
	if err != nil {
		return 0, nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-errCh:
		return 0, nil, fmt.Errorf("port-forward to %s/%s: %w", g.namespace, pod, err)
	case <-ctx.Done():
		close(stopCh)
		return 0, nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopCh)
		return 0, nil, fmt.Errorf("resolving local port: %w", err)
	}
	return int(ports[0].Local), func() { close(stopCh) }, nil
}

// pickReadyPod returns a Ready pod of the query-server deployment.
func pickReadyPod(ctx context.Context, client kubernetes.Interface, namespace string) (string, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=ollie,app.kubernetes.io/component=query",
	})
	if err != nil {
		return "", fmt.Errorf("listing query-server pods: %w", err)
	}
	var ready []string
	for _, p := range pods.Items {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = append(ready, p.Name)
			}
		}
	}
	if len(ready) == 0 {
		return "", fmt.Errorf("no Ready query-server pod in namespace %s (is ollie installed?)", namespace)
	}
	sort.Strings(ready)
	return ready[0], nil
}
