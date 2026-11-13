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
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/cmd/opentelemetry-sink/pb"
	"github.com/parquet-go/parquet-go"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/gcsblob"
	_ "gocloud.dev/blob/s3blob"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

const (
	fileMagic   uint32 = 0x5042494E // "PBIN"
	fileVersion uint32 = 1
)

type TypeCode uint32

const (
	TypeCode_Unknown    TypeCode = 0
	TypeCode_ObjectType TypeCode = 1
)

// Config represents the configurations for the compactor.
type Config struct {
	ArchiveURL      string
	SettleDelay     time.Duration
	RowGroupSize    int
	FileSizeLimit   int64
	MaxShardsPerRun int
	TmpDir          string
}

type partitionKey struct {
	date string
	hour string
}

// OpenBucketWithPrefix opens a gocloud bucket, wrapping it with PrefixedBucket if there's a prefix path.
func OpenBucketWithPrefix(ctx context.Context, urlStr string) (*blob.Bucket, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	scheme := u.Scheme
	if scheme == "" {
		scheme = "file"
	}

	if scheme == "file" {
		// For file, the entire path is the root of the bucket
		return blob.OpenBucket(ctx, urlStr)
	}

	bucketName := u.Host
	prefix := strings.TrimPrefix(u.Path, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	rootURL := fmt.Sprintf("%s://%s", scheme, bucketName)
	if u.RawQuery != "" {
		rootURL += "?" + u.RawQuery
	}
	rootBucket, err := blob.OpenBucket(ctx, rootURL)
	if err != nil {
		return nil, err
	}

	if prefix != "" {
		return blob.PrefixedBucket(rootBucket, prefix), nil
	}
	return rootBucket, nil
}

// Compact runs a single compaction cycle.
func Compact(ctx context.Context, cfg *Config) error {
	bucket, err := OpenBucketWithPrefix(ctx, cfg.ArchiveURL)
	if err != nil {
		return fmt.Errorf("failed to open bucket: %w", err)
	}
	defer bucket.Close()

	// 1. List raw shards
	log.Println("Listing raw shards...")
	rawShards := make(map[string]*blob.ListObject)
	iter := bucket.List(&blob.ListOptions{
		Prefix: "raw/",
	})
	for {
		obj, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to list raw shards: %w", err)
		}
		if !obj.IsDir && strings.HasSuffix(obj.Key, ".bin") {
			rawShards[obj.Key] = obj
		}
	}

	// 2. List compacted markers
	log.Println("Listing compacted markers...")
	compactedMarkers := make(map[string]bool)
	compIter := bucket.List(&blob.ListOptions{
		Prefix: "compacted/",
	})
	for {
		obj, err := compIter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to list compacted markers: %w", err)
		}
		if !obj.IsDir && strings.HasSuffix(obj.Key, ".compacted") {
			compactedMarkers[obj.Key] = true
		}
	}

	// 3. Find eligible shards
	cutoffTime := time.Now().Add(-cfg.SettleDelay)
	var eligibleKeys []string

	for key, obj := range rawShards {
		parts := strings.Split(key, "/")
		if len(parts) < 3 || parts[0] != "raw" {
			continue
		}
		pod := parts[1]
		filename := parts[2]
		markerKey := fmt.Sprintf("compacted/%s/%s.compacted", pod, filename)

		if compactedMarkers[markerKey] {
			continue
		}

		// Determine creation timestamp
		tsStr := strings.TrimSuffix(strings.TrimPrefix(filename, "shard-"), ".bin")
		var nano int64
		var shardTime time.Time
		if _, err := fmt.Sscanf(tsStr, "%d", &nano); err == nil {
			shardTime = time.Unix(0, nano)
		} else {
			shardTime = obj.ModTime
		}

		if shardTime.Before(cutoffTime) {
			eligibleKeys = append(eligibleKeys, key)
		}
	}

	if len(eligibleKeys) == 0 {
		log.Println("No shards eligible for compaction.")
		return nil
	}

	sort.Strings(eligibleKeys)
	if len(eligibleKeys) > cfg.MaxShardsPerRun {
		eligibleKeys = eligibleKeys[:cfg.MaxShardsPerRun]
	}

	log.Printf("Selected %d shards for compaction.", len(eligibleKeys))

	// 4. Download and parse eligible shards
	var logRows []LogRow
	var traceRows []TraceRow
	var metricRows []MetricRow
	totalSkippedHistograms := 0

	for _, key := range eligibleKeys {
		log.Printf("Downloading and parsing shard: %s", key)
		data, err := bucket.ReadAll(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to read shard %s: %w", key, err)
		}

		messages, err := parseShard(ctx, bytes.NewReader(data))
		if err != nil {
			log.Printf("Warning: failed to parse shard %s: %v. Skipping.", key, err)
			continue
		}

		for _, msg := range messages {
			switch req := msg.(type) {
			case *collogspb.ExportLogsServiceRequest:
				logRows = append(logRows, extractLogs(req)...)
			case *coltracepb.ExportTraceServiceRequest:
				traceRows = append(traceRows, extractTraces(req)...)
			case *colmetricspb.ExportMetricsServiceRequest:
				rows, skipped := extractMetrics(req)
				metricRows = append(metricRows, rows...)
				totalSkippedHistograms += skipped
			default:
				log.Printf("Warning: unrecognized message type %T in shard. Skipping.", req)
			}
		}
	}

	if totalSkippedHistograms > 0 {
		log.Printf("Skipped %d histogram/summary metric points (out of scope for this version).", totalSkippedHistograms)
	}

	// 5. Group rows by partition key
	logsByPartition := make(map[partitionKey][]LogRow)
	tracesByPartition := make(map[partitionKey][]TraceRow)
	metricsByPartition := make(map[partitionKey][]MetricRow)

	for _, row := range logRows {
		pk := partitionKey{
			date: row.Timestamp.UTC().Format("2006-01-02"),
			hour: row.Timestamp.UTC().Format("15"),
		}
		logsByPartition[pk] = append(logsByPartition[pk], row)
	}

	for _, row := range traceRows {
		pk := partitionKey{
			date: row.Timestamp.UTC().Format("2006-01-02"),
			hour: row.Timestamp.UTC().Format("15"),
		}
		tracesByPartition[pk] = append(tracesByPartition[pk], row)
	}

	for _, row := range metricRows {
		pk := partitionKey{
			date: row.Timestamp.UTC().Format("2006-01-02"),
			hour: row.Timestamp.UTC().Format("15"),
		}
		metricsByPartition[pk] = append(metricsByPartition[pk], row)
	}

	// 6. Sort and write partitions
	// Logs partitions
	for pk, rows := range logsByPartition {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].ServiceName != rows[j].ServiceName {
				return rows[i].ServiceName < rows[j].ServiceName
			}
			return rows[i].Timestamp.Before(rows[j].Timestamp)
		})

		log.Printf("Writing %d log rows for partition date=%s/hour=%s", len(rows), pk.date, pk.hour)
		err := writePartitionData(ctx, bucket, "logs", pk, rows, cfg,
			parquet.BloomFilters(
				parquet.SplitBlockFilter(10, "TraceId"),
				parquet.SplitBlockFilter(10, "K8sPod"),
			),
		)
		if err != nil {
			return fmt.Errorf("failed to write logs partition: %w", err)
		}
	}

	// Traces partitions
	for pk, rows := range tracesByPartition {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].ServiceName != rows[j].ServiceName {
				return rows[i].ServiceName < rows[j].ServiceName
			}
			return rows[i].Timestamp.Before(rows[j].Timestamp)
		})

		log.Printf("Writing %d trace rows for partition date=%s/hour=%s", len(rows), pk.date, pk.hour)
		err := writePartitionData(ctx, bucket, "traces", pk, rows, cfg,
			parquet.BloomFilters(
				parquet.SplitBlockFilter(10, "TraceId"),
				parquet.SplitBlockFilter(10, "K8sPod"),
			),
		)
		if err != nil {
			return fmt.Errorf("failed to write traces partition: %w", err)
		}
	}

	// Metrics partitions
	for pk, rows := range metricsByPartition {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].ServiceName != rows[j].ServiceName {
				return rows[i].ServiceName < rows[j].ServiceName
			}
			return rows[i].Timestamp.Before(rows[j].Timestamp)
		})

		log.Printf("Writing %d metric rows for partition date=%s/hour=%s", len(rows), pk.date, pk.hour)
		err := writePartitionData(ctx, bucket, "metrics", pk, rows, cfg,
			parquet.BloomFilters(
				parquet.SplitBlockFilter(10, "K8sPod"),
			),
		)
		if err != nil {
			return fmt.Errorf("failed to write metrics partition: %w", err)
		}
	}

	// 7. Write compacted marker files to commit the transactions
	log.Println("Writing compaction markers for successfully compacted shards...")
	for _, key := range eligibleKeys {
		parts := strings.Split(key, "/")
		pod := parts[1]
		filename := parts[2]
		markerKey := fmt.Sprintf("compacted/%s/%s.compacted", pod, filename)

		if err := bucket.WriteAll(ctx, markerKey, []byte("compacted"), nil); err != nil {
			return fmt.Errorf("failed to write marker key %s: %w", markerKey, err)
		}
	}

	log.Printf("Successfully completed compaction of %d shards.", len(eligibleKeys))
	return nil
}

// writePartitionData handles writing sorted slice of rows of type T to split Parquet parts in the bucket.
func writePartitionData[T any](ctx context.Context, bucket *blob.Bucket, signal string, pk partitionKey, rows []T, cfg *Config, writerOpts ...parquet.WriterOption) error {
	if len(rows) == 0 {
		return nil
	}

	var startIdx int
	partIdx := 0
	for startIdx < len(rows) {
		nextIdx, err := func() (int, error) {
			tmpFile, err := os.CreateTemp(cfg.TmpDir, "otel-compactor-*.parquet")
			if err != nil {
				return 0, fmt.Errorf("failed to create temp file: %w", err)
			}
			tmpPath := tmpFile.Name()
			defer os.Remove(tmpPath)

			// Set up parquet writer options, including default sort columns: ServiceName, Timestamp
			allOpts := append([]parquet.WriterOption{
				parquet.SchemaOf(new(T)),
				parquet.Compression(&parquet.Zstd),
				parquet.MaxRowsPerRowGroup(int64(cfg.RowGroupSize)),
				parquet.SortingWriterConfig(
					parquet.SortingColumns(
						parquet.Ascending("ServiceName"),
						parquet.Ascending("Timestamp"),
					),
				),
			}, writerOpts...)

			writer := parquet.NewWriter(tmpFile, allOpts...)

			var i int
			for i = startIdx; i < len(rows); i++ {
				if err := writer.Write(rows[i]); err != nil {
					tmpFile.Close()
					return 0, fmt.Errorf("failed to write parquet row: %w", err)
				}

				// Periodically check size after flush
				if (i-startIdx+1)%5000 == 0 {
					if err := writer.Flush(); err != nil {
						tmpFile.Close()
						return 0, fmt.Errorf("failed to flush parquet writer: %w", err)
					}
					fi, err := tmpFile.Stat()
					if err == nil && fi.Size() >= cfg.FileSizeLimit {
						i++ // move pointer to next row and stop
						break
					}
				}
			}

			if err := writer.Close(); err != nil {
				tmpFile.Close()
				return 0, fmt.Errorf("failed to close parquet writer: %w", err)
			}
			tmpFile.Close()

			// Read and upload the file
			partData, err := os.ReadFile(tmpPath)
			if err != nil {
				return 0, fmt.Errorf("failed to read temporary parquet file: %w", err)
			}

			timestampNano := time.Now().UnixNano()
			outputKey := fmt.Sprintf("parquet/signal=%s/date=%s/hour=%s/part-%d-%d.parquet",
				signal, pk.date, pk.hour, timestampNano, partIdx)

			if err := bucket.WriteAll(ctx, outputKey, partData, nil); err != nil {
				return 0, fmt.Errorf("failed to write parquet part to bucket: %w", err)
			}

			return i, nil
		}()
		if err != nil {
			return err
		}
		startIdx = nextIdx
		partIdx++
	}

	return nil
}

// parseShard parses a binary raw shard from reader r.
func parseShard(ctx context.Context, r io.Reader) ([]proto.Message, error) {
	fileHeader := make([]byte, 16)
	n, err := io.ReadFull(r, fileHeader)
	if err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}

	if n < 16 || binary.BigEndian.Uint32(fileHeader[0:4]) != fileMagic {
		return nil, fmt.Errorf("invalid shard magic header")
	}

	version := binary.BigEndian.Uint32(fileHeader[4:8])
	if version > fileVersion {
		return nil, fmt.Errorf("unsupported shard version %d (max supported %d)", version, fileVersion)
	}

	typeByCode := make(map[TypeCode]string)
	var results []proto.Message

	for {
		header := make([]byte, 16)
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}

		length := binary.BigEndian.Uint32(header[0:4])
		expectedChecksum := binary.BigEndian.Uint32(header[4:8])
		typeCode := TypeCode(binary.BigEndian.Uint32(header[12:16]))

		data := make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}

		if crc32.ChecksumIEEE(data) != expectedChecksum {
			return nil, fmt.Errorf("CRC32 checksum mismatch: expected %x, got %x", expectedChecksum, crc32.ChecksumIEEE(data))
		}

		if typeCode == TypeCode_ObjectType {
			obj := &pb.ObjectType{}
			if err := obj.Unmarshal(data); err == nil {
				typeByCode[TypeCode(obj.TypeCode)] = obj.TypeName
			}
			continue
		}

		typeName, ok := typeByCode[typeCode]
		if !ok {
			continue
		}

		msg, err := createMessage(typeName)
		if err != nil {
			continue
		}

		if err := proto.Unmarshal(data, msg); err != nil {
			return nil, err
		}

		results = append(results, msg)
	}

	return results, nil
}

func createMessage(typeName string) (proto.Message, error) {
	switch typeName {
	case "opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest":
		return &coltracepb.ExportTraceServiceRequest{}, nil
	case "opentelemetry.proto.collector.metrics.v1.ExportMetricsServiceRequest":
		return &colmetricspb.ExportMetricsServiceRequest{}, nil
	case "opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest":
		return &collogspb.ExportLogsServiceRequest{}, nil
	default:
		return nil, fmt.Errorf("unknown type: %s", typeName)
	}
}

func extractLogs(req *collogspb.ExportLogsServiceRequest) []LogRow {
	var rows []LogRow
	for _, rl := range req.ResourceLogs {
		resSchema := rl.SchemaUrl
		resAttrs := rl.Resource.Attributes
		for _, sl := range rl.ScopeLogs {
			scopeName := sl.Scope.Name
			scopeVer := sl.Scope.Version
			scopeAttrs := sl.Scope.Attributes
			for _, lr := range sl.LogRecords {
				ts := time.Unix(0, int64(lr.TimeUnixNano))
				if lr.TimeUnixNano == 0 {
					ts = time.Unix(0, int64(lr.ObservedTimeUnixNano))
				}
				serviceName, k8sNamespace, k8sPod := ExtractPromoted(resAttrs, scopeAttrs, lr.Attributes)

				bodyStr := AnyValueToString(lr.Body)

				rows = append(rows, LogRow{
					Timestamp:          ts,
					TraceId:            hex.EncodeToString(lr.TraceId),
					SpanId:             hex.EncodeToString(lr.SpanId),
					TraceFlags:         lr.Flags,
					SeverityText:       lr.SeverityText,
					SeverityNumber:     int32(lr.SeverityNumber),
					ServiceName:        serviceName,
					K8sNamespace:       k8sNamespace,
					K8sPod:             k8sPod,
					Body:               bodyStr,
					ResourceSchemaUrl:  resSchema,
					ResourceAttributes: AttributesToMap(resAttrs),
					ScopeName:          scopeName,
					ScopeVersion:       scopeVer,
					ScopeAttributes:    AttributesToMap(scopeAttrs),
					LogAttributes:      AttributesToMap(lr.Attributes),
				})
			}
		}
	}
	return rows
}

func extractTraces(req *coltracepb.ExportTraceServiceRequest) []TraceRow {
	var rows []TraceRow
	for _, rs := range req.ResourceSpans {
		resAttrs := rs.Resource.Attributes
		for _, ss := range rs.ScopeSpans {
			scopeName := ss.Scope.Name
			scopeVer := ss.Scope.Version
			scopeAttrs := ss.Scope.Attributes
			for _, span := range ss.Spans {
				serviceName, k8sNamespace, k8sPod := ExtractPromoted(resAttrs, scopeAttrs, span.Attributes)

				ts := time.Unix(0, int64(span.StartTimeUnixNano))
				duration := int64(span.EndTimeUnixNano - span.StartTimeUnixNano)

				var events []SpanEvent
				for _, ev := range span.Events {
					events = append(events, SpanEvent{
						Timestamp:  time.Unix(0, int64(ev.TimeUnixNano)),
						Name:       ev.Name,
						Attributes: AttributesToMap(ev.Attributes),
					})
				}

				var links []SpanLink
				for _, l := range span.Links {
					links = append(links, SpanLink{
						TraceId:    hex.EncodeToString(l.TraceId),
						SpanId:     hex.EncodeToString(l.SpanId),
						TraceState: l.TraceState,
						Attributes: AttributesToMap(l.Attributes),
					})
				}

				statusCode := ""
				statusMsg := ""
				if span.Status != nil {
					statusCode = span.Status.Code.String()
					statusMsg = span.Status.Message
				}

				rows = append(rows, TraceRow{
					Timestamp:          ts,
					Duration:           duration,
					TraceId:            hex.EncodeToString(span.TraceId),
					SpanId:             hex.EncodeToString(span.SpanId),
					ParentSpanId:       hex.EncodeToString(span.ParentSpanId),
					TraceState:         span.TraceState,
					SpanName:           span.Name,
					SpanKind:           span.Kind.String(),
					ServiceName:        serviceName,
					K8sNamespace:       k8sNamespace,
					K8sPod:             k8sPod,
					ResourceAttributes: AttributesToMap(resAttrs),
					ScopeName:          scopeName,
					ScopeVersion:       scopeVer,
					SpanAttributes:     AttributesToMap(span.Attributes),
					StatusCode:         statusCode,
					StatusMessage:      statusMsg,
					Events:             events,
					Links:              links,
				})
			}
		}
	}
	return rows
}

func extractMetrics(req *colmetricspb.ExportMetricsServiceRequest) ([]MetricRow, int) {
	var rows []MetricRow
	skippedHistograms := 0

	for _, rm := range req.ResourceMetrics {
		resAttrs := rm.Resource.Attributes
		for _, sm := range rm.ScopeMetrics {
			scopeName := sm.Scope.Name
			scopeVer := sm.Scope.Version
			scopeAttrs := sm.Scope.Attributes
			for _, m := range sm.Metrics {
				metricName := m.Name
				metricDesc := m.Description
				metricUnit := m.Unit

				var metricType string
				var aggTemporality string
				var isMonotonic bool
				var points []*metricspb.NumberDataPoint

				if m.GetGauge() != nil {
					metricType = "gauge"
					points = m.GetGauge().DataPoints
				} else if m.GetSum() != nil {
					metricType = "sum"
					sum := m.GetSum()
					aggTemporality = sum.AggregationTemporality.String()
					isMonotonic = sum.IsMonotonic
					points = sum.DataPoints
				} else {
					skippedHistograms++
					continue
				}

				for _, dp := range points {
					serviceName, k8sNamespace, k8sPod := ExtractPromoted(resAttrs, scopeAttrs, dp.Attributes)

					var val float64
					switch v := dp.Value.(type) {
					case *metricspb.NumberDataPoint_AsDouble:
						val = v.AsDouble
					case *metricspb.NumberDataPoint_AsInt:
						val = float64(v.AsInt)
					}

					rows = append(rows, MetricRow{
						Timestamp:              time.Unix(0, int64(dp.TimeUnixNano)),
						StartTimestamp:         time.Unix(0, int64(dp.StartTimeUnixNano)),
						MetricName:             metricName,
						MetricDescription:      metricDesc,
						MetricUnit:             metricUnit,
						MetricType:             metricType,
						AggregationTemporality: aggTemporality,
						IsMonotonic:            isMonotonic,
						Value:                  val,
						Flags:                  uint32(dp.Flags),
						ServiceName:            serviceName,
						K8sNamespace:           k8sNamespace,
						K8sPod:                 k8sPod,
						ResourceAttributes:     AttributesToMap(resAttrs),
						ScopeName:              scopeName,
						ScopeVersion:           scopeVer,
						Attributes:             AttributesToMap(dp.Attributes),
					})
				}
			}
		}
	}
	return rows, skippedHistograms
}
