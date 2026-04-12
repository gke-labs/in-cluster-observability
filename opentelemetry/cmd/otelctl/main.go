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
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: %s <logs|traces|metrics> [filter1] [filter2] ...", os.Args[0])
	}

	tableName := strings.ToUpper(os.Args[1])
	var table pb.Table
	switch tableName {
	case "LOGS":
		table = pb.Table_LOGS
	case "TRACES":
		table = pb.Table_TRACES
	case "METRICS":
		table = pb.Table_METRICS
	default:
		log.Fatalf("Invalid table: %s. Must be logs, traces, or metrics", tableName)
	}

	filters := os.Args[2:]

	kubeConfigFlags := genericclioptions.NewConfigFlags(true)
	config, err := kubeConfigFlags.ToRESTConfig()
	if err != nil {
		log.Fatalf("Failed to get kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	namespace := "observability-system"

	pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=opentelemetry-query-server",
	})
	if err != nil {
		log.Fatalf("Failed to find query server pods: %v", err)
	}

	if len(pods.Items) == 0 {
		log.Fatalf("No query server pods found in namespace %s", namespace)
	}

	pod := pods.Items[0]

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(pod.Name).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		log.Fatalf("Failed to create spdy roundtripper: %v", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	readyCh := make(chan struct{})
	stopCh := make(chan struct{})
	defer close(stopCh)

	// Pick an available port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("Failed to listen on ephemeral port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close() // Release the port so portforward can bind to it

	ports := []string{fmt.Sprintf("%d:9443", port)}
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)

	fw, err := portforward.New(dialer, ports, stopCh, readyCh, out, errOut)
	if err != nil {
		log.Fatalf("Failed to create portforward: %v", err)
	}

	go func() {
		if err := fw.ForwardPorts(); err != nil {
			log.Fatalf("Port forward failed: %v", err)
		}
	}()

	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		log.Fatalf("Timed out waiting for port forward to be ready. errOut: %s", errOut.String())
	}

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect to query server: %v", err)
	}
	defer conn.Close()

	client := pb.NewFrontendQueryServiceClient(conn)

	resp, err := client.Query(context.Background(), &pb.FrontendQueryRequest{
		Table:   table,
		Filters: filters,
	})
	if err != nil {
		log.Fatalf("Query failed: %v", err)
	}

	for _, result := range resp.Results {
		fmt.Println(string(result))
	}
}
