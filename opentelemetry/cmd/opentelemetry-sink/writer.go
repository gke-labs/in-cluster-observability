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
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/cmd/opentelemetry-sink/pb"
	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/async"
	pkgpb "github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/pb"
	"github.com/gke-labs/in-cluster-observability/opentelemetry/pkg/store"
	"k8s.io/klog/v2"
)

type TypeCode uint32

const (
	TypeCode_Unknown    TypeCode = 0
	TypeCode_ObjectType TypeCode = 1
)

const (
	fileMagic   uint32 = 0x5042494E // "PBIN"
	fileVersion uint32 = 1
)

type Writer struct {
	dir          string
	fileMutex    sync.Mutex
	f            *os.File
	currentShard string

	typeCodesMutex sync.Mutex
	nextTypeCode   TypeCode
	typeCodes      map[string]TypeCode

	stopChan chan struct{}

	// Uploader fields
	archiveURL     string
	localRetention time.Duration
	podName        string
	archiveStore   store.ArchiveStore
	uploader       *async.TaskRunner
	cleanupMutex   sync.Mutex
}

func NewWriter(dir string, archiveURL string, localRetention time.Duration) (*Writer, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	pod := os.Getenv("POD_NAME")
	if pod == "" {
		var err error
		pod, err = os.Hostname()
		if err != nil {
			pod = "unknown-pod"
		}
	}

	w := &Writer{
		dir:            dir,
		nextTypeCode:   32,
		typeCodes:      make(map[string]TypeCode),
		stopChan:       make(chan struct{}),
		archiveURL:     archiveURL,
		localRetention: localRetention,
		podName:        pod,
	}

	if w.archiveURL != "" {
		ctx := context.Background()
		archiveStore, err := store.NewArchiveStore(ctx, archiveURL)
		if err != nil {
			return nil, fmt.Errorf("failed to open archive store at %s: %w", archiveURL, err)
		}
		w.archiveStore = archiveStore
		w.uploader = async.NewTaskRunner(3)
	}

	if err := w.rotateShard(); err != nil {
		if w.archiveStore != nil {
			_ = w.archiveStore.Close()
		}
		return nil, err
	}

	if w.archiveURL != "" {
		w.enqueueCatchUp()
	}

	go w.shardingLoop()

	return w, nil
}

func (w *Writer) shardingLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := w.rotateShard(); err != nil {
				log.Printf("failed to rotate shard: %v", err)
			}
			if w.archiveURL != "" {
				go w.runRetentionCleanup(context.Background())
			}
		case <-w.stopChan:
			return
		}
	}
}

func (w *Writer) rotateShard() error {
	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()

	var oldShard string
	if w.f != nil {
		syncErr := w.f.Sync()
		closeErr := w.f.Close()
		if syncErr != nil {
			return fmt.Errorf("failed to sync shard: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("failed to close shard: %w", closeErr)
		}
		oldShard = w.currentShard
	}

	shardName := filepath.Join(w.dir, fmt.Sprintf("shard-%020d.bin", time.Now().UnixNano()))
	f, err := os.OpenFile(shardName, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	w.f = f
	w.currentShard = shardName

	// Write the file header
	fileHeader := make([]byte, 16)
	binary.BigEndian.PutUint32(fileHeader[0:4], fileMagic)
	binary.BigEndian.PutUint32(fileHeader[4:8], fileVersion)
	if _, err := w.f.Write(fileHeader); err != nil {
		return err
	}

	// Re-write all known type codes to the new shard so it's self-contained.
	w.typeCodesMutex.Lock()
	defer w.typeCodesMutex.Unlock()

	// Record the mapping for ObjectType itself.
	objType := &pb.ObjectType{
		TypeCode: uint32(TypeCode_ObjectType),
		TypeName: "otlptracefile.ObjectType",
	}
	data := objType.Marshal()
	if err := w.writeBytesWithTypeCodeLocked(TypeCode_ObjectType, data); err != nil {
		return err
	}

	for typeName, code := range w.typeCodes {
		obj := &pb.ObjectType{
			TypeCode: uint32(code),
			TypeName: typeName,
		}
		if err := w.writeBytesWithTypeCodeLocked(TypeCode_ObjectType, obj.Marshal()); err != nil {
			return err
		}
	}

	if oldShard != "" && w.archiveURL != "" {
		w.enqueueUpload(oldShard)
	}

	return nil
}

func (w *Writer) Close() error {
	close(w.stopChan)
	w.fileMutex.Lock()
	var oldShard string
	if w.f != nil {
		if err := w.f.Sync(); err != nil {
			log.Printf("warning: failed to sync current shard on close: %v", err)
		}
		if err := w.f.Close(); err != nil {
			log.Printf("warning: failed to close current shard: %v", err)
		}
		oldShard = w.currentShard
		w.f = nil
	}
	w.fileMutex.Unlock()

	if oldShard != "" && w.archiveURL != "" {
		w.enqueueUpload(oldShard)
	}

	if w.archiveURL != "" {
		w.uploader.Close()
		if w.archiveStore != nil {
			if err := w.archiveStore.Close(); err != nil {
				log.Printf("warning: failed to close archive bucket: %v", err)
			}
		}
	}
	return nil
}

func (w *Writer) enqueueUpload(shardPath string) {
	err := w.uploader.Submit(func() {
		w.processUploadWithRetry(shardPath)
	})
	if err != nil {
		log.Printf("warning: failed to submit upload for shard %s: %v", shardPath, err)
	}
}

func (w *Writer) enqueueCatchUp() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		log.Printf("failed to read dir for crash catch-up: %v", err)
		return
	}

	w.fileMutex.Lock()
	current := w.currentShard
	w.fileMutex.Unlock()

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "shard-") && strings.HasSuffix(entry.Name(), ".bin") {
			fullPath := filepath.Join(w.dir, entry.Name())
			if fullPath != current {
				log.Printf("found leftover shard on startup, enqueuing for upload: %s", fullPath)
				w.enqueueUpload(fullPath)
			}
		}
	}
}

func (w *Writer) uploadShard(ctx context.Context, shardPath string) error {
	shardName := filepath.Base(shardPath)
	key := "raw/" + w.podName + "/" + shardName

	if err := w.archiveStore.Upload(ctx, key, shardPath); err != nil {
		return fmt.Errorf("failed to upload shard: %w", err)
	}

	log.Printf("successfully uploaded shard %s to %s", shardPath, key)
	return nil
}

func (w *Writer) processUploadWithRetry(shardPath string) {
	if _, err := os.Stat(shardPath); os.IsNotExist(err) {
		log.Printf("shard path %s does not exist, skipping upload", shardPath)
		return
	}

	pending := w.uploader.TaskCount()
	log.Printf("starting upload for %s. pending uploads in queue: %d", shardPath, pending)

	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := w.uploadShard(ctx, shardPath)
		cancel()

		if err == nil {
			w.runRetentionCleanup(context.Background())
			return
		}

		log.Printf("failed to upload shard %s: %v. Retrying in %v...", shardPath, err, backoff)

		select {
		case <-w.stopChan:
			log.Printf("writer is stopping; aborting active upload retry for %s. Shard remains on local disk.", shardPath)
			return
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (w *Writer) runRetentionCleanup(ctx context.Context) {
	if w.archiveURL == "" {
		return
	}

	log := klog.FromContext(ctx)

	w.cleanupMutex.Lock()
	defer w.cleanupMutex.Unlock()

	w.fileMutex.Lock()
	current := w.currentShard
	w.fileMutex.Unlock()

	now := time.Now()
	err := filepath.WalkDir(w.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "shard-") || !strings.HasSuffix(name, ".bin") {
			return nil
		}
		if path == current {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		if now.Sub(info.ModTime()) < w.localRetention {
			return nil
		}

		ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
		key := "raw/" + w.podName + "/" + name
		uploaded, err := w.archiveStore.IsUploaded(ctxTimeout, key, path)
		cancel()

		if err == nil && uploaded {
			if err := os.Remove(path); err != nil {
				log.Error(err, "failed to remove local shard", "path", path)
			} else {
				log.Info("removed local shard (retention expired and upload verified)", "path", path)
			}
		} else if err != nil {
			log.Error(err, "keeping local shard; retention expired but remote check failed", "path", path)
		} else {
			log.Info("keeping local shard; retention expired but not uploaded", "path", path)
		}
		return nil
	})

	if err != nil {
		log.Error(err, "failed to walk dir for retention cleanup")
	}
}

func (w *Writer) WriteObject(ctx context.Context, obj proto.Message) error {
	typeName := string(obj.ProtoReflect().Descriptor().FullName())
	code, err := w.codeForType(ctx, typeName)
	if err != nil {
		return err
	}

	data, err := proto.Marshal(obj)
	if err != nil {
		return err
	}

	return w.writeBytesWithTypeCode(ctx, code, data)
}

func (w *Writer) codeForType(ctx context.Context, typeName string) (TypeCode, error) {
	w.typeCodesMutex.Lock()
	if code, ok := w.typeCodes[typeName]; ok {
		w.typeCodesMutex.Unlock()
		return code, nil
	}
	code := w.nextTypeCode
	w.nextTypeCode++
	w.typeCodes[typeName] = code
	w.typeCodesMutex.Unlock()

	// Record the type mapping
	objType := &pb.ObjectType{
		TypeCode: uint32(code),
		TypeName: typeName,
	}
	data := objType.Marshal()
	if err := w.writeBytesWithTypeCode(ctx, TypeCode_ObjectType, data); err != nil {
		return 0, err
	}

	return code, nil
}

func (w *Writer) writeBytesWithTypeCode(ctx context.Context, typeCode TypeCode, data []byte) error {
	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()
	return w.writeBytesWithTypeCodeLocked(typeCode, data)
}

func (w *Writer) writeBytesWithTypeCodeLocked(typeCode TypeCode, data []byte) error {
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(data))
	binary.BigEndian.PutUint32(header[8:12], 0) // Flags
	binary.BigEndian.PutUint32(header[12:16], uint32(typeCode))

	if _, err := w.f.Write(header); err != nil {
		return err
	}
	if _, err := w.f.Write(data); err != nil {
		return err
	}
	return nil
}

func (w *Writer) Query(ctx context.Context, query string) ([]proto.Message, error) {
	filters := make(map[string]string)
	if query != "" {
		for _, part := range strings.Split(query, ";") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 {
				filters[kv[0]] = kv[1]
			}
		}
	}

	targetMetric := filters["metric"]
	targetNamespace := filters["namespace"]
	targetPod := filters["pod"]
	latestOnly := filters["latest_only"] == "true"

	// Flush current shard so we can read from it
	w.fileMutex.Lock()
	if w.f != nil {
		w.f.Sync()
	}
	w.fileMutex.Unlock()

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "shard-") && strings.HasSuffix(entry.Name(), ".bin") {
			files = append(files, filepath.Join(w.dir, entry.Name()))
		}
	}
	sort.Strings(files)

	var results []proto.Message
	type podKey struct {
		namespace string
		podName   string
	}
	latestMetrics := make(map[podKey]*colmetricspb.ExportMetricsServiceRequest)

	for _, file := range files {
		func() {
			f, err := os.Open(file)
			if err != nil {
				log.Printf("failed to open shard %s for reading: %v", file, err)
				return
			}
			defer f.Close()

			fileHeader := make([]byte, 16)
			n, err := io.ReadFull(f, fileHeader)
			if err != nil {
				if err == io.EOF {
					return
				}
			}

			if n < 16 || binary.BigEndian.Uint32(fileHeader[0:4]) != fileMagic {
				log.Printf("warning: shard file %s has missing or incorrect magic; treating as legacy version 0", file)
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					log.Printf("failed to seek to start of legacy shard file %s: %v", file, err)
					return
				}
			} else {
				version := binary.BigEndian.Uint32(fileHeader[4:8])
				if version > fileVersion {
					log.Printf("warning: shard file %s has unsupported version %d (max supported %d); skipping", file, version, fileVersion)
					return
				}
			}

			typeByCode := make(map[TypeCode]string)

			for {
				header := make([]byte, 16)
				if _, err := io.ReadFull(f, header); err != nil {
					if err != io.EOF && err != io.ErrUnexpectedEOF {
						log.Printf("failed to read header from %s: %v", file, err)
					}
					break
				}

				length := binary.BigEndian.Uint32(header[0:4])
				expectedChecksum := binary.BigEndian.Uint32(header[4:8])
				typeCode := TypeCode(binary.BigEndian.Uint32(header[12:16]))

				data := make([]byte, length)
				if _, err := io.ReadFull(f, data); err != nil {
					if err != io.EOF && err != io.ErrUnexpectedEOF {
						log.Printf("failed to read data from %s: %v", file, err)
					}
					break
				}

				if crc32.ChecksumIEEE(data) != expectedChecksum {
					log.Printf("warning: CRC32 mismatch reading shard %s: expected %x, got %x", file, expectedChecksum, crc32.ChecksumIEEE(data))
					break
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
					log.Printf("error creating message for type %s: %v", typeName, err)
					continue
				}

				if err := proto.Unmarshal(data, msg); err != nil {
					log.Printf("error unmarshaling message for type %s: %v", typeName, err)
					continue
				}

				if mreq, ok := msg.(*colmetricspb.ExportMetricsServiceRequest); ok {
					if latestOnly {
						for _, rm := range mreq.ResourceMetrics {
							if matchesResource(rm, targetNamespace, targetPod) {
								if targetMetric == "" || matchesMetricName(rm, targetMetric) {
									var resPodName, resNamespace string
									for _, attr := range rm.Resource.Attributes {
										if attr.Key == "k8s.pod.name" {
											resPodName = attr.Value.GetStringValue()
										} else if attr.Key == "k8s.namespace.name" {
											resNamespace = attr.Value.GetStringValue()
										}
									}
									if resPodName != "" {
										key := podKey{namespace: resNamespace, podName: resPodName}
										latestMetrics[key] = mreq
									}
								}
							}
						}
					} else {
						if matchesMetrics(mreq, targetMetric, targetNamespace, targetPod) {
							results = append(results, mreq)
						}
					}
				} else if targetMetric == "" && targetNamespace == "" && targetPod == "" {
					results = append(results, msg)
				}
			}
		}()
	}

	if latestOnly {
		seen := make(map[*colmetricspb.ExportMetricsServiceRequest]bool)
		for _, mreq := range latestMetrics {
			if !seen[mreq] {
				results = append(results, mreq)
				seen[mreq] = true
			}
		}
	}

	return results, nil
}

func matchesMetrics(req *colmetricspb.ExportMetricsServiceRequest, targetMetric, targetNamespace, targetPod string) bool {
	for _, rm := range req.ResourceMetrics {
		if !matchesResource(rm, targetNamespace, targetPod) {
			continue
		}
		if targetMetric == "" || matchesMetricName(rm, targetMetric) {
			return true
		}
	}
	return false
}

func matchesResource(rm *metricspb.ResourceMetrics, targetNamespace, targetPod string) bool {
	if targetNamespace == "" && (targetPod == "" || targetPod == "*") {
		return true
	}
	podName := ""
	namespace := ""
	for _, attr := range rm.Resource.Attributes {
		if attr.Key == "k8s.pod.name" {
			podName = attr.Value.GetStringValue()
		} else if attr.Key == "k8s.namespace.name" {
			namespace = attr.Value.GetStringValue()
		}
	}
	if targetNamespace != "" && namespace != targetNamespace {
		return false
	}
	if targetPod != "" && targetPod != "*" && podName != targetPod {
		return false
	}
	return true
}

func matchesMetricName(rm *metricspb.ResourceMetrics, targetMetric string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == targetMetric {
				return true
			}
		}
	}
	return false
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

func anyValueString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		if val.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	default:
		return v.String()
	}
}

func globMatch(pattern, val string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == val
	}
	if !strings.HasPrefix(val, parts[0]) {
		return false
	}
	val = val[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(val, parts[i])
		if idx == -1 {
			return false
		}
		val = val[idx+len(parts[i]):]
	}
	return strings.HasSuffix(val, parts[len(parts)-1])
}

func getAttributeValue(rl *logspb.ResourceLogs, lr *logspb.LogRecord, key string) (string, bool) {
	if rl.Resource != nil {
		for _, attr := range rl.Resource.Attributes {
			if attr.GetKey() == key {
				return anyValueString(attr.GetValue()), true
			}
		}
	}
	for _, attr := range lr.Attributes {
		if attr.GetKey() == key {
			return anyValueString(attr.GetValue()), true
		}
	}
	return "", false
}

func parseShardNanos(name string) (int64, error) {
	base := filepath.Base(name)
	if !strings.HasPrefix(base, "shard-") || !strings.HasSuffix(base, ".bin") {
		return 0, fmt.Errorf("invalid shard name: %s", name)
	}
	nanosStr := base[len("shard-") : len(base)-len(".bin")]
	var nanos int64
	_, err := fmt.Sscanf(nanosStr, "%d", &nanos)
	if err != nil {
		return 0, err
	}
	return nanos, nil
}

type matchedLog struct {
	timestamp int64
	data      []byte
}

func (w *Writer) SearchLogs(ctx context.Context, req *pkgpb.SearchLogsRequest) ([][]byte, error) {
	// Flush current shard so we can read from it
	w.fileMutex.Lock()
	if w.f != nil {
		w.f.Sync()
	}
	w.fileMutex.Unlock()

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, err
	}

	type shardInfo struct {
		path  string
		nanos int64
	}

	var shards []shardInfo
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "shard-") && strings.HasSuffix(entry.Name(), ".bin") {
			nanos, err := parseShardNanos(entry.Name())
			if err != nil {
				continue
			}
			shards = append(shards, shardInfo{
				path:  filepath.Join(w.dir, entry.Name()),
				nanos: nanos,
			})
		}
	}

	// Sort shards ascending so that we can easily find the start and end of each shard's duration
	sort.Slice(shards, func(i, j int) bool {
		return shards[i].nanos < shards[j].nanos
	})

	var candidateShards []string
	for i, sh := range shards {
		// Shard starts at sh.nanos.
		// If shard starts after query end time + 5m, we skip it.
		if sh.nanos > req.EndTimeUnixNano+int64(5*time.Minute) {
			continue
		}

		// If there is a next shard, this shard ends when the next one starts.
		// If next shard starts before query start time - 5m, this shard ended before query start, so we skip it.
		if i+1 < len(shards) {
			nextStart := shards[i+1].nanos
			if nextStart+int64(5*time.Minute) < req.StartTimeUnixNano {
				continue
			}
		}

		candidateShards = append(candidateShards, sh.path)
	}

	var matches []matchedLog

	for _, file := range candidateShards {
		err := func() error {
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()

			fileHeader := make([]byte, 16)
			n, err := io.ReadFull(f, fileHeader)
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}

			if n < 16 || binary.BigEndian.Uint32(fileHeader[0:4]) != fileMagic {
				// Legacy file, seek to start
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return err
				}
			} else {
				version := binary.BigEndian.Uint32(fileHeader[4:8])
				if version > fileVersion {
					return nil
				}
			}

			typeByCode := make(map[TypeCode]string)

			for {
				header := make([]byte, 16)
				if _, err := io.ReadFull(f, header); err != nil {
					break
				}

				length := binary.BigEndian.Uint32(header[0:4])
				expectedChecksum := binary.BigEndian.Uint32(header[4:8])
				typeCode := TypeCode(binary.BigEndian.Uint32(header[12:16]))

				data := make([]byte, length)
				if _, err := io.ReadFull(f, data); err != nil {
					break
				}

				if crc32.ChecksumIEEE(data) != expectedChecksum {
					break
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

				if typeName != "opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest" {
					continue
				}

				msg := &collogspb.ExportLogsServiceRequest{}
				if err := proto.Unmarshal(data, msg); err != nil {
					continue
				}

				for _, rl := range msg.ResourceLogs {
					for _, sl := range rl.ScopeLogs {
						for _, lr := range sl.LogRecords {
							ts := int64(lr.TimeUnixNano)
							if ts == 0 {
								ts = int64(lr.ObservedTimeUnixNano)
							}

							if ts < req.StartTimeUnixNano || ts > req.EndTimeUnixNano {
								continue
							}

							bodyStr := anyValueString(lr.Body)
							bodyStrLower := strings.ToLower(bodyStr)
							bodyMatched := true
							for _, term := range req.BodyContains {
								if !strings.Contains(bodyStrLower, strings.ToLower(term)) {
									bodyMatched = false
									break
								}
							}
							if !bodyMatched {
								continue
							}

							attrsMatched := true
							for _, filter := range req.Attributes {
								if filter.Key == "SeverityText" {
									if !globMatch(strings.ToLower(filter.Value), strings.ToLower(lr.SeverityText)) {
										attrsMatched = false
										break
									}
								} else {
									val, found := getAttributeValue(rl, lr, filter.Key)
									if !found {
										attrsMatched = false
										break
									}
									if !globMatch(filter.Value, val) {
										attrsMatched = false
										break
									}
								}
							}
							if !attrsMatched {
								continue
							}

							singleReq := &collogspb.ExportLogsServiceRequest{
								ResourceLogs: []*logspb.ResourceLogs{
									{
										Resource:  rl.Resource,
										SchemaUrl: rl.SchemaUrl,
										ScopeLogs: []*logspb.ScopeLogs{
											{
												Scope:      sl.Scope,
												SchemaUrl:  sl.SchemaUrl,
												LogRecords: []*logspb.LogRecord{lr},
											},
										},
									},
								},
							}

							b, err := proto.Marshal(singleReq)
							if err != nil {
								continue
							}

							matches = append(matches, matchedLog{
								timestamp: ts,
								data:      b,
							})
						}
					}
				}
			}
			return nil
		}()
		if err != nil {
			log.Printf("error reading shard %s: %v", file, err)
		}
	}

	// Sort globally newest-first
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].timestamp > matches[j].timestamp
	})

	limit := int(req.Limit)
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}

	var results [][]byte
	for _, m := range matches {
		results = append(results, m.data)
	}

	return results, nil
}
