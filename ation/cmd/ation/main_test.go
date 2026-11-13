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
	"os"
	"path/filepath"
	"testing"
)

func TestRunCommand(t *testing.T) {
	contentDir := t.TempDir()
	staticDir := t.TempDir()
	outDir := t.TempDir()

	// Create a dummy page
	pagePath := filepath.Join(contentDir, "index.md")
	if err := os.WriteFile(pagePath, []byte("---\ntitle: Home\n---\n# Hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a dummy static asset
	staticPath := filepath.Join(staticDir, "robots.txt")
	if err := os.WriteFile(staticPath, []byte("User-agent: *\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("flags with build subcommand", func(t *testing.T) {
		dest := filepath.Join(outDir, "sub1")
		err := run([]string{"build", "-content", contentDir, "-static", staticDir, "-out", dest})
		if err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "index.html")); err != nil {
			t.Errorf("missing index.html: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "robots.txt")); err != nil {
			t.Errorf("missing robots.txt: %v", err)
		}
	})

	t.Run("positional args", func(t *testing.T) {
		dest := filepath.Join(outDir, "sub2")
		err := run([]string{contentDir, dest, staticDir})
		if err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "index.html")); err != nil {
			t.Errorf("missing index.html: %v", err)
		}
	})

	t.Run("missing required arguments", func(t *testing.T) {
		err := run([]string{})
		if err == nil {
			t.Fatal("expected error on empty args, got nil")
		}
	})
}
