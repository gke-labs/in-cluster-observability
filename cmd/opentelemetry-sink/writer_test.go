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
	"io"
	"os"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

func TestWriter(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "otel-test-*.bin")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	writer, err := NewWriter(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to create writer: %v", err)
	}
	defer writer.Close()

	ctx := context.Background()
	req := &coltracepb.ExportTraceServiceRequest{}
	if err := writer.WriteObject(ctx, req); err != nil {
		t.Fatalf("failed to write object: %v", err)
	}

	// Read back and verify
	f, err := os.Open(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to open output file: %v", err)
	}
	defer f.Close()

	// 1st record should be ObjectType for ObjectType
	header := make([]byte, 16)
	if _, err := io.ReadFull(f, header); err != nil {
		t.Fatalf("failed to read 1st header: %v", err)
	}
	typeCode := binary.BigEndian.Uint32(header[12:16])
	if typeCode != uint32(TypeCode_ObjectType) {
		t.Errorf("expected typeCode %d, got %d", TypeCode_ObjectType, typeCode)
	}
	length := binary.BigEndian.Uint32(header[0:4])
	f.Seek(int64(length), io.SeekCurrent)

	// 2nd record should be ObjectType for ExportTraceServiceRequest
	if _, err := io.ReadFull(f, header); err != nil {
		t.Fatalf("failed to read 2nd header: %v", err)
	}
	typeCode = binary.BigEndian.Uint32(header[12:16])
	if typeCode != uint32(TypeCode_ObjectType) {
		t.Errorf("expected typeCode %d (ObjectType), got %d", TypeCode_ObjectType, typeCode)
	}
	length = binary.BigEndian.Uint32(header[0:4])
	f.Seek(int64(length), io.SeekCurrent)

	// 3rd record should be ExportTraceServiceRequest
	if _, err := io.ReadFull(f, header); err != nil {
		t.Fatalf("failed to read 3rd header: %v", err)
	}
	length = binary.BigEndian.Uint32(header[0:4])
	typeCode = binary.BigEndian.Uint32(header[12:16])
	if typeCode != 32 {
		t.Errorf("expected typeCode 32, got %d", typeCode)
	}

}
