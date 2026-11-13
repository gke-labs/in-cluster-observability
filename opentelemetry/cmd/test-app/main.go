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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func sendLog(ctx context.Context, client collogspb.LogsServiceClient, namespace, podName, serviceName, body string) {
	req := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{
							Key: "k8s.namespace.name",
							Value: &commonpb.AnyValue{
								Value: &commonpb.AnyValue_StringValue{StringValue: namespace},
							},
						},
						{
							Key: "k8s.pod.name",
							Value: &commonpb.AnyValue{
								Value: &commonpb.AnyValue_StringValue{StringValue: podName},
							},
						},
						{
							Key: "service.name",
							Value: &commonpb.AnyValue{
								Value: &commonpb.AnyValue_StringValue{StringValue: serviceName},
							},
						},
					},
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						LogRecords: []*logspb.LogRecord{
							{
								TimeUnixNano: uint64(time.Now().UnixNano()),
								SeverityText: "INFO",
								Body: &commonpb.AnyValue{
									Value: &commonpb.AnyValue_StringValue{StringValue: body},
								},
							},
						},
					},
				},
			},
		},
	}
	ctxTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := client.Export(ctxTimeout, req); err != nil {
		log.Printf("failed to export log: %v", err)
	}
}

func main() {
	endpoint := flag.String("endpoint", "opentelemetry-node-agent.observability-system:4317", "OTLP endpoint")
	addr := flag.String("addr", ":8080", "HTTP server address")
	flag.Parse()

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = "test-app-pod"
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("test-app"),
			semconv.K8SPodNameKey.String(podName),
			semconv.K8SNamespaceNameKey.String(namespace),
		),
	)
	if err != nil {
		log.Fatalf("failed to create resource: %v", err)
	}

	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithEndpoint(*endpoint),
	)
	if err != nil {
		log.Fatalf("failed to create exporter: %v", err)
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(5*time.Second))),
	)
	defer meterProvider.Shutdown(ctx)
	otel.SetMeterProvider(meterProvider)

	meter := otel.Meter("test-app")

	var requestCount int64
	var qps int64

	_, err = meter.Int64ObservableGauge("qps",
		metric.WithDescription("Current QPS of the application"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(atomic.LoadInt64(&qps))
			return nil
		}),
	)
	if err != nil {
		log.Fatalf("failed to create gauge: %v", err)
	}

	go func() {
		lastCount := int64(0)
		ticker := time.NewTicker(1 * time.Second)
		for range ticker.C {
			currentCount := atomic.LoadInt64(&requestCount)
			atomic.StoreInt64(&qps, currentCount-lastCount)
			lastCount = currentCount
		}
	}()

	logsConn, err := grpc.NewClient(*endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect to log sink: %v", err)
	}
	defer logsConn.Close()
	logsClient := collogspb.NewLogsServiceClient(logsConn)

	logChan := make(chan string, 1000)
	go func() {
		for msg := range logChan {
			sendLog(context.Background(), logsClient, namespace, podName, "test-app", msg)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		select {
		case logChan <- fmt.Sprintf("HTTP %s %s request handled", r.Method, r.URL.Path):
		default:
		}
		fmt.Fprintf(w, "OK")
	})

	log.Printf("test-app starting, sending metrics to %s, listening on %s", *endpoint, *addr)
	sendLog(ctx, logsClient, namespace, podName, "test-app", fmt.Sprintf("HTTP test-app starting, sending metrics to %s, listening on %s", *endpoint, *addr))
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
}
