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
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type mockRunner struct {
	called bool
	cfg    Config
	err    error
}

func (m *mockRunner) Run(_ context.Context, cfg Config) error {
	m.called = true
	m.cfg = cfg
	return m.err
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		want        Config
		expectErr   bool
		errContains string
	}{
		{
			name: "valid flags",
			args: []string{"build", "-src", "docs/site", "-out", "docs/site/dist", "-image", "docs-builder"},
			want: Config{
				Src:   "docs/site",
				Out:   "docs/site/dist",
				Image: "docs-builder",
			},
		},
		{
			name: "custom excludes flag",
			args: []string{"build", "-src", "docs/site", "-out", "docs/site/dist", "-image", "docs-builder", "-exclude", "node_modules,custom_cache"},
			want: Config{
				Src:      "docs/site",
				Out:      "docs/site/dist",
				Image:    "docs-builder",
				Excludes: []string{"node_modules", "custom_cache"},
			},
		},
		{
			name:        "no arguments",
			args:        []string{},
			expectErr:   true,
			errContains: "expected 'build' subcommand",
		},
		{
			name:        "unknown subcommand",
			args:        []string{"invalid"},
			expectErr:   true,
			errContains: "unknown command \"invalid\"",
		},
		{
			name:        "missing image flag",
			args:        []string{"build", "-src", "docs/site", "-out", "docs/site/dist"},
			expectErr:   true,
			errContains: "missing required flags",
		},
		{
			name:        "missing out flag",
			args:        []string{"build", "-src", "docs/site", "-image", "docs-builder"},
			expectErr:   true,
			errContains: "missing required flags",
		},
		{
			name:        "missing src flag",
			args:        []string{"build", "-out", "docs/site/dist", "-image", "docs-builder"},
			expectErr:   true,
			errContains: "missing required flags",
		},
		{
			name:        "unknown flag reports flag error",
			args:        []string{"build", "-unknown"},
			expectErr:   true,
			errContains: "flag provided but not defined: -unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var errBuf bytes.Buffer
			got, err := parseArgs(tc.args, &errBuf)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("parseArgs(%v) expected error, got nil", tc.args)
				}
				combinedErr := err.Error() + " " + errBuf.String()
				if !strings.Contains(combinedErr, tc.errContains) {
					t.Errorf("error %q does not contain %q", combinedErr, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%v) unexpected error: %v", tc.args, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestValidatePaths(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "docs", "site")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	markerFile := filepath.Join(srcDir, "marker.txt")
	if err := os.WriteFile(markerFile, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("out is same as src", func(t *testing.T) {
		_, _, err := validatePaths(srcDir, srcDir)
		if err == nil || !strings.Contains(err.Error(), "cannot be the source directory") {
			t.Fatalf("expected 'cannot be the source directory' error, got %v", err)
		}
	})

	t.Run("out contains src", func(t *testing.T) {
		parentDir := filepath.Join(tmpDir, "docs")
		_, _, err := validatePaths(srcDir, parentDir)
		if err == nil || !strings.Contains(err.Error(), "cannot contain source directory") {
			t.Fatalf("expected 'cannot contain source directory' error, got %v", err)
		}
	})

	t.Run("out root contains src", func(t *testing.T) {
		_, _, err := validatePaths(srcDir, tmpDir)
		if err == nil || !strings.Contains(err.Error(), "cannot contain source directory") {
			t.Fatalf("expected 'cannot contain source directory' error, got %v", err)
		}
	})

	t.Run("out is subdirectory of src", func(t *testing.T) {
		subDir := filepath.Join(srcDir, "dist")
		srcAbs, outAbs, err := validatePaths(srcDir, subDir)
		if err != nil {
			t.Fatalf("unexpected error for subdirectory output: %v", err)
		}
		if srcAbs == "" || outAbs == "" {
			t.Fatal("expected non-empty absolute paths")
		}
	})

	t.Run("out is disjoint directory", func(t *testing.T) {
		disjointDir := filepath.Join(tmpDir, "out")
		srcAbs, outAbs, err := validatePaths(srcDir, disjointDir)
		if err != nil {
			t.Fatalf("unexpected error for disjoint output: %v", err)
		}
		if srcAbs == "" || outAbs == "" {
			t.Fatal("expected non-empty absolute paths")
		}
	})

	t.Run("src does not exist", func(t *testing.T) {
		_, _, err := validatePaths(filepath.Join(tmpDir, "nonexistent"), filepath.Join(tmpDir, "out"))
		if err == nil {
			t.Fatal("expected error for nonexistent src, got nil")
		}
	})

	t.Run("src is not a directory", func(t *testing.T) {
		_, _, err := validatePaths(markerFile, filepath.Join(tmpDir, "out"))
		if err == nil || !strings.Contains(err.Error(), "is not a directory") {
			t.Fatalf("expected 'is not a directory' error, got %v", err)
		}
	})

	t.Run("runner refuses and preserves source", func(t *testing.T) {
		r := &DockerRunner{Stderr: &bytes.Buffer{}}
		err := r.Run(t.Context(), Config{
			Src:   srcDir,
			Out:   srcDir,
			Image: "dummy-image",
		})
		if err == nil {
			t.Fatal("expected runner to fail when out == src")
		}
		if _, err := os.Stat(markerFile); err != nil {
			t.Fatalf("marker file in source was deleted: %v", err)
		}
	})
}

func TestComputeExcludes(t *testing.T) {
	srcAbs := "/work/docs/site"
	t.Run("out inside src is excluded", func(t *testing.T) {
		outAbs := "/work/docs/site/dist-ation"
		excludes := computeExcludes(srcAbs, outAbs, []string{"node_modules"})
		expected := []string{".git", "node_modules", "dist-ation"}
		if !reflect.DeepEqual(excludes, expected) {
			t.Errorf("got %v, want %v", excludes, expected)
		}
	})

	t.Run("disjoint out is not added to excludes", func(t *testing.T) {
		outAbs := "/work/out"
		excludes := computeExcludes(srcAbs, outAbs, nil)
		expected := []string{".git"}
		if !reflect.DeepEqual(excludes, expected) {
			t.Errorf("got %v, want %v", excludes, expected)
		}
	})
}

func TestWriteTarStream(t *testing.T) {
	tmpDir := t.TempDir()

	filesToCreate := map[string]string{
		"index.html":           "<h1>Hello</h1>",
		"content/dist/post.md": "# Post under dist",
		"node_modules/bad.js":  "console.log('bad')",
		"dist/old.html":        "old",
		".astro/cache.bin":     "cache",
		".git/HEAD":            "ref: refs/heads/main",
	}

	for relPath, content := range filesToCreate {
		fullPath := filepath.Join(tmpDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	excludes := []string{".git", "node_modules", "dist", ".astro"}
	var buf bytes.Buffer
	if err := writeTarStream(tmpDir, excludes, &buf); err != nil {
		t.Fatalf("writeTarStream failed: %v", err)
	}

	tr := tar.NewReader(&buf)
	foundEntries := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		foundEntries[hdr.Name] = true
	}

	if !foundEntries["src/index.html"] {
		t.Error("expected src/index.html in tar")
	}
	if !foundEntries["src/content/dist/post.md"] {
		t.Error("expected src/content/dist/post.md (nested dist) in tar")
	}

	for entry := range foundEntries {
		if strings.HasPrefix(entry, "src/node_modules") {
			t.Errorf("unexpected entry in tar: %s", entry)
		}
		if entry == "src/dist" || strings.HasPrefix(entry, "src/dist/") {
			t.Errorf("unexpected entry in tar: %s", entry)
		}
		if strings.HasPrefix(entry, "src/.astro") {
			t.Errorf("unexpected entry in tar: %s", entry)
		}
		if strings.HasPrefix(entry, "src/.git") {
			t.Errorf("unexpected entry in tar: %s", entry)
		}
	}
}

func TestCopyIn_CommandFailsEarly(t *testing.T) {
	tmpDir := t.TempDir()
	largeData := strings.Repeat("test content\n", 1000)
	if err := os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte(largeData), 0644); err != nil {
		t.Fatal(err)
	}

	r := &DockerRunner{Engine: "false"}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.copyIn(ctx, "false", "nonexistent-container", tmpDir, nil)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from failing copyIn command, got nil")
		}
	case <-ctx.Done():
		t.Fatal("copyIn hung when copy command exited early")
	}
}

func TestRunSuccess(t *testing.T) {
	runner := &mockRunner{}
	args := []string{"build", "-src", "src", "-out", "out", "-image", "img"}
	err := run(t.Context(), args, runner, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !runner.called {
		t.Fatal("expected runner to be called")
	}
	want := Config{Src: "src", Out: "out", Image: "img"}
	if !reflect.DeepEqual(runner.cfg, want) {
		t.Errorf("runner got cfg %+v, want %+v", runner.cfg, want)
	}
}

func TestRunFailure(t *testing.T) {
	expectedErr := errors.New("docker failure")
	runner := &mockRunner{err: expectedErr}
	args := []string{"build", "-src", "src", "-out", "out", "-image", "img"}
	err := run(t.Context(), args, runner, &bytes.Buffer{})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("got err %v, want %v", err, expectedErr)
	}
}
