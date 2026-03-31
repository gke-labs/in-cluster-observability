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
	"sync"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/gke-labs/in-cluster-observability/opentelemetry/cmd/opentelemetry-sink/pb"
)

type TypeCode uint32

const (
	TypeCode_Unknown    TypeCode = 0
	TypeCode_ObjectType TypeCode = 1
)

type Writer struct {
	fileMutex sync.Mutex
	f         *os.File

	typeCodesMutex sync.Mutex
	nextTypeCode   TypeCode
	typeCodes      map[string]TypeCode
}

func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		f:            f,
		nextTypeCode: 32,
		typeCodes:    make(map[string]TypeCode),
	}

	// Record the mapping for ObjectType itself.
	objType := &pb.ObjectType{
		TypeCode: uint32(TypeCode_ObjectType),
		TypeName: "otlptracefile.ObjectType",
	}
	data := objType.Marshal()
	if err := w.writeBytesWithTypeCode(context.Background(), TypeCode_ObjectType, data); err != nil {
		f.Close()
		return nil, err
	}

	return w, nil
}

func (w *Writer) Close() error {
	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()
	return w.f.Close()
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
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(data))
	binary.BigEndian.PutUint32(header[8:12], 0) // Flags
	binary.BigEndian.PutUint32(header[12:16], uint32(typeCode))

	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()

	if _, err := w.f.Write(header); err != nil {
		return err
	}
	if _, err := w.f.Write(data); err != nil {
		return err
	}
	return nil
}

func (w *Writer) Query(ctx context.Context, query string) ([]proto.Message, error) {
	// For now, return all objects in the file since the query logic is not yet defined.
	// But in a real implementation, we would filter based on the 'query' string.
	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()

	if _, err := w.f.Seek(0, 0); err != nil {
		return nil, err
	}

	// We need a way to map TypeCode back to proto.Message types.
	// We'll have to use the typeCodes mapping.
	w.typeCodesMutex.Lock()
	typeByCode := make(map[TypeCode]string)
	for name, code := range w.typeCodes {
		typeByCode[code] = name
	}
	w.typeCodesMutex.Unlock()

	var results []proto.Message
	for {
		header := make([]byte, 16)
		if _, err := io.ReadFull(w.f, header); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		length := binary.BigEndian.Uint32(header[0:4])
		// crc := binary.BigEndian.Uint32(header[4:8])
		// flags := binary.BigEndian.Uint32(header[8:12])
		typeCode := TypeCode(binary.BigEndian.Uint32(header[12:16]))

		data := make([]byte, length)
		if _, err := io.ReadFull(w.f, data); err != nil {
			return nil, err
		}

		if typeCode == TypeCode_ObjectType {
			// Skip type mapping objects for now, as they are already in memory.
			continue
		}

		typeName, ok := typeByCode[typeCode]
		if !ok {
			continue
		}

		// Use the proto registry or some other mechanism to create a message of the correct type.
		// For simplicity, we'll assume we know the types we care about.
		// In a real implementation, we would use dynamic messages.
		msg, err := createMessage(typeName)
		if err != nil {
			log.Printf("error creating message for type %s: %v", typeName, err)
			continue
		}

		if err := proto.Unmarshal(data, msg); err != nil {
			log.Printf("error unmarshaling message for type %s: %v", typeName, err)
			continue
		}

		results = append(results, msg)
	}

	// Seek back to the end of the file for subsequent writes.
	if _, err := w.f.Seek(0, 2); err != nil {
		return nil, err
	}

	return results, nil
}

// In a real implementation, this would be more robust.
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
