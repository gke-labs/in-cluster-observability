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
	"strings"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/cmd/opentelemetry-sink/pb"
)

type TypeCode uint32

const (
	TypeCode_Unknown    TypeCode = 0
	TypeCode_ObjectType TypeCode = 1
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
}

func NewWriter(dir string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	w := &Writer{
		dir:          dir,
		nextTypeCode: 32,
		typeCodes:    make(map[string]TypeCode),
		stopChan:     make(chan struct{}),
	}

	if err := w.rotateShard(); err != nil {
		return nil, err
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
		case <-w.stopChan:
			return
		}
	}
}

func (w *Writer) rotateShard() error {
	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()

	if w.f != nil {
		w.f.Close()
	}

	shardName := filepath.Join(w.dir, fmt.Sprintf("shard-%020d.bin", time.Now().UnixNano()))
	f, err := os.OpenFile(shardName, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	w.f = f
	w.currentShard = shardName

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

	return nil
}

func (w *Writer) Close() error {
	close(w.stopChan)
	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()
	if w.f != nil {
		return w.f.Close()
	}
	return nil
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

			typeByCode := make(map[TypeCode]string)

			for {
				header := make([]byte, 16)
				if _, err := io.ReadFull(f, header); err != nil {
					if err != io.EOF {
						log.Printf("failed to read header from %s: %v", file, err)
					}
					break
				}

				length := binary.BigEndian.Uint32(header[0:4])
				typeCode := TypeCode(binary.BigEndian.Uint32(header[12:16]))

				data := make([]byte, length)
				if _, err := io.ReadFull(f, data); err != nil {
					log.Printf("failed to read data from %s: %v", file, err)
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
