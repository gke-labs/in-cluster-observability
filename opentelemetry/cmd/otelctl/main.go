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
	"log"
	"net"
	"os"
	"strings"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/klient"
	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	namespace := "observability-system"

	pods, err := klient.ListPods(namespace, "app=opentelemetry-query-server")
	if err != nil {
		log.Fatalf("Failed to find query server pods: %v", err)
	}

	if len(pods) == 0 {
		log.Fatalf("No query server pods found in namespace %s", namespace)
	}

	podName := pods[0]

	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		return klient.Dial(namespace, podName, 9443)
	}

	conn, err := grpc.DialContext(
		context.Background(),
		"in-process-transport",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
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
