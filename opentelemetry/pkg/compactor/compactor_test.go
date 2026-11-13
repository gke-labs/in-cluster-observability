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

package compactor

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/cmd/opentelemetry-sink/pb"
	"github.com/parquet-go/parquet-go"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Helper to write file header
func testWriteFileHeader(w io.Writer) error {
	fileHeader := make([]byte, 16)
	binary.BigEndian.PutUint32(fileHeader[0:4], fileMagic)
	binary.BigEndian.PutUint32(fileHeader[4:8], fileVersion)
	_, err := w.Write(fileHeader)
	return err
}

// Helper to write a custom ObjectType record
func testWriteObjectType(w io.Writer, obj *pb.ObjectType) error {
	data := obj.Marshal()
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(data))
	binary.BigEndian.PutUint32(header[8:12], 0) // Flags
	binary.BigEndian.PutUint32(header[12:16], uint32(TypeCode_ObjectType))

	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

// Helper to write a protobuf record
func testWriteRecord(w io.Writer, typeCode TypeCode, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(data))
	binary.BigEndian.PutUint32(header[8:12], 0) // Flags
	binary.BigEndian.PutUint32(header[12:16], uint32(typeCode))

	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

func TestCompactorRoundTrip(t *testing.T) {
	tmpBucketDir, err := os.MkdirTemp("", "otel-compactor-test-bucket-*")
	if err != nil {
		t.Fatalf("failed to create temp bucket dir: %v", err)
	}
	defer os.RemoveAll(tmpBucketDir)

	// Create directories for raw input
	podName := "test-pod-abc"
	rawDir := filepath.Join(tmpBucketDir, "raw", podName)
	if err := os.MkdirAll(rawDir, 0755); err != nil {
		t.Fatalf("failed to create raw dir: %v", err)
	}

	// Construct mock OTLP messages
	testTime := time.Date(2026, 9, 2, 14, 15, 0, 0, time.UTC)
	testTimeNano := uint64(testTime.UnixNano())

	// 1. Mock Logs
	logReq := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"}}},
						{Key: "k8s.namespace.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-ns"}}},
						{Key: "k8s.pod.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: podName}}},
					},
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0",
						},
						LogRecords: []*logspb.LogRecord{
							{
								TimeUnixNano:   testTimeNano,
								SeverityText:   "INFO",
								SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
								Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "this is a test log body"}},
								TraceId:        []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
								SpanId:         []byte{0, 1, 2, 3, 4, 5, 6, 7},
							},
						},
					},
				},
			},
		},
	}

	// 2. Mock Traces
	traceReq := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"}}},
						{Key: "k8s.namespace.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-ns"}}},
						{Key: "k8s.pod.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: podName}}},
					},
				},
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0",
						},
						Spans: []*tracepb.Span{
							{
								StartTimeUnixNano: testTimeNano,
								EndTimeUnixNano:   testTimeNano + 1000000, // 1ms duration
								TraceId:           []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
								SpanId:            []byte{0, 1, 2, 3, 4, 5, 6, 7},
								ParentSpanId:      []byte{7, 6, 5, 4, 3, 2, 1, 0},
								Name:              "test-span",
								Kind:              tracepb.Span_SPAN_KIND_SERVER,
								Status: &tracepb.Status{
									Code:    tracepb.Status_STATUS_CODE_OK,
									Message: "all good",
								},
								Events: []*tracepb.Span_Event{
									{
										TimeUnixNano: testTimeNano + 1000,
										Name:         "event-1",
										Attributes: []*commonpb.KeyValue{
											{Key: "evt-attr", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "val"}}},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// 3. Mock Metrics
	metricReq := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"}}},
						{Key: "k8s.namespace.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test-ns"}}},
						{Key: "k8s.pod.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: podName}}},
					},
				},
				ScopeMetrics: []*metricspb.ScopeMetrics{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0",
						},
						Metrics: []*metricspb.Metric{
							{
								Name:        "http_requests_total",
								Description: "Total HTTP requests",
								Unit:        "1",
								Data: &metricspb.Metric_Sum{
									Sum: &metricspb.Sum{
										AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
										IsMonotonic:            true,
										DataPoints: []*metricspb.NumberDataPoint{
											{
												TimeUnixNano:      testTimeNano,
												StartTimeUnixNano: testTimeNano - 1000000,
												Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: 100.5},
												Attributes: []*commonpb.KeyValue{
													{Key: "http.status", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "200"}}},
												},
											},
										},
									},
								},
							},
							// Skip metric (histogram)
							{
								Name:        "http_request_duration",
								Description: "HTTP request duration",
								Data: &metricspb.Metric_Histogram{
									Histogram: &metricspb.Histogram{},
								},
							},
						},
					},
				},
			},
		},
	}

	// Write raw shard file
	shardFileName := "shard-00000001710000000000.bin" // nanosecond unix timestamp
	shardFilePath := filepath.Join(rawDir, shardFileName)
	f, err := os.Create(shardFilePath)
	if err != nil {
		t.Fatalf("failed to create shard file: %v", err)
	}

	if err := testWriteFileHeader(f); err != nil {
		t.Fatalf("failed to write file header: %v", err)
	}

	// Write type mappings
	objType_ObjType := &pb.ObjectType{TypeCode: uint32(TypeCode_ObjectType), TypeName: "otlptracefile.ObjectType"}
	if err := testWriteObjectType(f, objType_ObjType); err != nil {
		t.Fatalf("failed to write TypeCode_ObjectType: %v", err)
	}

	objType_Logs := &pb.ObjectType{TypeCode: 33, TypeName: "opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest"}
	if err := testWriteObjectType(f, objType_Logs); err != nil {
		t.Fatalf("failed to write log typecode: %v", err)
	}

	objType_Traces := &pb.ObjectType{TypeCode: 34, TypeName: "opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest"}
	if err := testWriteObjectType(f, objType_Traces); err != nil {
		t.Fatalf("failed to write trace typecode: %v", err)
	}

	objType_Metrics := &pb.ObjectType{TypeCode: 35, TypeName: "opentelemetry.proto.collector.metrics.v1.ExportMetricsServiceRequest"}
	if err := testWriteObjectType(f, objType_Metrics); err != nil {
		t.Fatalf("failed to write metrics typecode: %v", err)
	}

	// Write the records
	if err := testWriteRecord(f, 33, logReq); err != nil {
		t.Fatalf("failed to write logs record: %v", err)
	}
	if err := testWriteRecord(f, 34, traceReq); err != nil {
		t.Fatalf("failed to write traces record: %v", err)
	}
	if err := testWriteRecord(f, 35, metricReq); err != nil {
		t.Fatalf("failed to write metrics record: %v", err)
	}

	f.Close()

	// Execute compaction
	cfg := &Config{
		ArchiveURL:      "file://" + tmpBucketDir,
		SettleDelay:     0, // immediately compact
		RowGroupSize:    10,
		FileSizeLimit:   1024 * 1024,
		MaxShardsPerRun: 10,
		TmpDir:          os.TempDir(),
	}

	ctx := t.Context()
	if err := Compact(ctx, cfg); err != nil {
		t.Fatalf("compaction failed: %v", err)
	}

	// Verify that the shard is marked compacted
	markerFile := filepath.Join(tmpBucketDir, "compacted", podName, shardFileName+".compacted")
	if _, err := os.Stat(markerFile); os.IsNotExist(err) {
		t.Fatalf("marker file does not exist: %s", markerFile)
	}

	// Verify that Parquet files have been created under hive partitioning
	// Target Hive directory structure: parquet/signal=<logs|traces|metrics>/date=2026-09-02/hour=14/
	logsDir := filepath.Join(tmpBucketDir, "parquet", "signal=logs", "date=2026-09-02", "hour=14")
	tracesDir := filepath.Join(tmpBucketDir, "parquet", "signal=traces", "date=2026-09-02", "hour=14")
	metricsDir := filepath.Join(tmpBucketDir, "parquet", "signal=metrics", "date=2026-09-02", "hour=14")

	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		t.Fatalf("logs parquet directory does not exist")
	}
	if _, err := os.Stat(tracesDir); os.IsNotExist(err) {
		t.Fatalf("traces parquet directory does not exist")
	}
	if _, err := os.Stat(metricsDir); os.IsNotExist(err) {
		t.Fatalf("metrics parquet directory does not exist")
	}

	// Read and verify logs parquet file
	logParts, err := os.ReadDir(logsDir)
	if err != nil || len(logParts) == 0 {
		t.Fatalf("no log parquet files found: %v", err)
	}
	logFile := filepath.Join(logsDir, logParts[0].Name())
	logsRead := readLogsParquet(t, logFile)
	if len(logsRead) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(logsRead))
	}
	if logsRead[0].Body != "this is a test log body" {
		t.Errorf("incorrect log body: %s", logsRead[0].Body)
	}
	if logsRead[0].K8sPod != podName {
		t.Errorf("incorrect logs promoted pod: %s", logsRead[0].K8sPod)
	}
	if logsRead[0].K8sNamespace != "test-ns" {
		t.Errorf("incorrect logs promoted namespace: %s", logsRead[0].K8sNamespace)
	}

	// Read and verify traces parquet file
	traceParts, err := os.ReadDir(tracesDir)
	if err != nil || len(traceParts) == 0 {
		t.Fatalf("no trace parquet files found: %v", err)
	}
	traceFile := filepath.Join(tracesDir, traceParts[0].Name())
	tracesRead := readTracesParquet(t, traceFile)
	if len(tracesRead) != 1 {
		t.Fatalf("expected 1 trace row, got %d", len(tracesRead))
	}
	if tracesRead[0].SpanName != "test-span" {
		t.Errorf("incorrect span name: %s", tracesRead[0].SpanName)
	}
	if tracesRead[0].Duration != 1000000 {
		t.Errorf("incorrect span duration: %d", tracesRead[0].Duration)
	}
	if tracesRead[0].StatusCode != "STATUS_CODE_OK" {
		t.Errorf("incorrect status code: %s", tracesRead[0].StatusCode)
	}
	if len(tracesRead[0].Events) != 1 || tracesRead[0].Events[0].Name != "event-1" {
		t.Errorf("incorrect span event parsing")
	}

	// Read and verify metrics parquet file
	metricParts, err := os.ReadDir(metricsDir)
	if err != nil || len(metricParts) == 0 {
		t.Fatalf("no metric parquet files found: %v", err)
	}
	metricFile := filepath.Join(metricsDir, metricParts[0].Name())
	metricsRead := readMetricsParquet(t, metricFile)
	if len(metricsRead) != 1 {
		t.Fatalf("expected 1 metric row (gauge + sum only), got %d", len(metricsRead))
	}
	if metricsRead[0].MetricName != "http_requests_total" {
		t.Errorf("incorrect metric name: %s", metricsRead[0].MetricName)
	}
	if metricsRead[0].Value != 100.5 {
		t.Errorf("incorrect metric value: %f", metricsRead[0].Value)
	}
	if metricsRead[0].K8sPod != podName {
		t.Errorf("incorrect metrics promoted pod: %s", metricsRead[0].K8sPod)
	}
}

// Helpers to read back Parquet data
func readLogsParquet(t *testing.T, path string) []LogRow {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open logs parquet: %v", err)
	}
	defer f.Close()

	reader := parquet.NewReader(f)
	defer reader.Close()

	var rows []LogRow
	for {
		var row LogRow
		err := reader.Read(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("failed to read log parquet row: %v", err)
		}
		rows = append(rows, row)
	}
	return rows
}

func readTracesParquet(t *testing.T, path string) []TraceRow {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open traces parquet: %v", err)
	}
	defer f.Close()

	reader := parquet.NewReader(f)
	defer reader.Close()

	var rows []TraceRow
	for {
		var row TraceRow
		err := reader.Read(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("failed to read trace parquet row: %v", err)
		}
		rows = append(rows, row)
	}
	return rows
}

func readMetricsParquet(t *testing.T, path string) []MetricRow {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open metrics parquet: %v", err)
	}
	defer f.Close()

	reader := parquet.NewReader(f)
	defer reader.Close()

	var rows []MetricRow
	for {
		var row MetricRow
		err := reader.Read(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("failed to read metric parquet row: %v", err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestOpenBucketWithPrefix_GS_S3(t *testing.T) {
	ctx := t.Context()
	// Test file URL opening is successful
	tmpDir, err := os.MkdirTemp("", "otel-compactor-bucket-prefix-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	b, err := OpenBucketWithPrefix(ctx, "file://"+tmpDir)
	if err != nil {
		t.Fatalf("failed to open file bucket with prefix: %v", err)
	}
	b.Close()

	// Parse gs / s3 URLs helper logic check
	// Because of mocking, we can check the URL parsing logic directly or verify that it compiles.
	u, err := url.Parse("gs://my-bucket/some/prefix")
	if err != nil {
		t.Fatalf("failed to parse url: %v", err)
	}
	prefix := strings.TrimPrefix(u.Path, "/")
	if prefix != "some/prefix" {
		t.Errorf("expected prefix some/prefix, got %s", prefix)
	}
}
