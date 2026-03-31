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
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

func main() {
	endpoint := flag.String("endpoint", "opentelemetry-node-agent.observability-system:4317", "OTLP endpoint")
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
	counter, err := meter.Int64Counter("test_metric",
		metric.WithDescription("A simple test metric"),
	)
	if err != nil {
		log.Fatalf("failed to create counter: %v", err)
	}

	log.Printf("test-app starting, sending metrics to %s", *endpoint)

	for {
		counter.Add(ctx, 10)
		log.Printf("incremented test_metric by 10")
		time.Sleep(1 * time.Second)
	}
}
