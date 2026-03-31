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
	"hash/crc32"
	"os"
	"sync"

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
