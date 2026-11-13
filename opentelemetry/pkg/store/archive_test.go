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

package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveStore_UploadAndCheck(t *testing.T) {
	ctx := t.Context()

	tmpDir, err := os.MkdirTemp("", "store-archive-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	archiveDir := filepath.Join(tmpDir, "archive")
	archiveURL := "file://" + archiveDir

	store, err := NewArchiveStore(ctx, archiveURL)
	if err != nil {
		t.Fatalf("failed to create ArchiveStore: %v", err)
	}
	defer store.Close()

	localFile := filepath.Join(tmpDir, "test.bin")
	content := []byte("hello world observations")
	if err := os.WriteFile(localFile, content, 0644); err != nil {
		t.Fatalf("failed to write local file: %v", err)
	}

	key := "raw/pod-1/shard-1.bin"

	// Initial upload
	if err := store.Upload(ctx, key, localFile); err != nil {
		t.Fatalf("failed to upload: %v", err)
	}

	// Verify file is uploaded on disk under correct path
	expectedPath := filepath.Join(archiveDir, key)
	got, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("failed to read expected uploaded file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("expected content %s, got %s", content, got)
	}

	// Verify IsUploaded works
	isUp, err := store.IsUploaded(ctx, key, localFile)
	if err != nil {
		t.Fatalf("failed to check upload status: %v", err)
	}
	if !isUp {
		t.Error("expected IsUploaded to return true")
	}

	// Modify local file to be different size
	if err := os.WriteFile(localFile, []byte("diff size"), 0644); err != nil {
		t.Fatalf("failed to modify local file: %v", err)
	}

	isUpDiff, err := store.IsUploaded(ctx, key, localFile)
	if err != nil {
		t.Fatalf("failed to check upload status after modification: %v", err)
	}
	if isUpDiff {
		t.Error("expected IsUploaded to return false after local file size changed")
	}
}
