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

package ation

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "docs", "site")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root containing docs/site starting from %s", dir)
		}
		dir = parent
	}
}

func TestBuildRealDocsSite(t *testing.T) {
	repoRoot := findRepoRoot(t)
	contentDir := filepath.Join(repoRoot, "docs", "site", "src", "content", "docs")
	staticDir := filepath.Join(repoRoot, "docs", "site", "public")
	outDir := t.TempDir()

	cfg := Config{
		ContentDir: contentDir,
		StaticDir:  staticDir,
		OutputDir:  outDir,
	}

	if err := Build(cfg); err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	expectedPaths := []string{
		"404.html",
		"architecture/index.html",
		"contributing/index.html",
		"docs/architecture/index.html",
		"docs/contributing/index.html",
		"docs/getting-started/index.html",
		"docs/index.html",
		"docs/roadmap/index.html",
		"docs/what-works-today/index.html",
		"favicon.svg",
		"getting-started/index.html",
		"index.html",
		"roadmap/index.html",
		"use-cases/hpa-autoscaling/index.html",
		"what-works-today/index.html",
	}

	var actualPaths []string
	err := filepath.Walk(outDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return err
		}
		actualPaths = append(actualPaths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking output directory: %v", err)
	}

	sort.Strings(expectedPaths)
	sort.Strings(actualPaths)

	if len(actualPaths) != len(expectedPaths) {
		t.Errorf("got %d files, want %d files.\nGot: %v\nWant: %v",
			len(actualPaths), len(expectedPaths), actualPaths, expectedPaths)
	}

	for i := range expectedPaths {
		if i >= len(actualPaths) {
			t.Errorf("missing expected output path: %s", expectedPaths[i])
			continue
		}
		if actualPaths[i] != expectedPaths[i] {
			t.Errorf("path mismatch at [%d]: got %s, want %s", i, actualPaths[i], expectedPaths[i])
		}
	}

	expectedTitles := map[string]string{
		"index.html":                           "Ollie",
		"404.html":                             "Not found",
		"architecture/index.html":              "Architecture",
		"contributing/index.html":              "Contributing",
		"getting-started/index.html":           "Getting started",
		"roadmap/index.html":                   "Roadmap",
		"use-cases/hpa-autoscaling/index.html": "Autoscale on captured traffic",
		"what-works-today/index.html":          "What works today",
	}

	for pageRel, expectedTitle := range expectedTitles {
		p := filepath.Join(outDir, filepath.FromSlash(pageRel))
		content, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("failed to read %s: %v", pageRel, err)
			continue
		}
		s := string(content)

		if !strings.HasPrefix(s, "<!DOCTYPE html>") {
			t.Errorf("%s: expected <!DOCTYPE html> preamble", pageRel)
		}
		if !strings.Contains(s, "<html lang=\"en\">") {
			t.Errorf("%s: expected <html lang=\"en\">", pageRel)
		}
		if !strings.Contains(s, "<meta charset=\"utf-8\">") {
			t.Errorf("%s: expected <meta charset=\"utf-8\">", pageRel)
		}

		expectedTitleTag := "<title>" + expectedTitle + "</title>"
		if !strings.Contains(s, expectedTitleTag) {
			t.Errorf("%s: expected %q, page content: %s", pageRel, expectedTitleTag, s[:min(300, len(s))])
		}

		if !strings.Contains(s, "<body>") || !strings.Contains(s, "</body>") {
			t.Errorf("%s: expected <body>...</body> tags", pageRel)
		}
	}

	// Verify home page MDX prose is not dropped
	homeContent, err := os.ReadFile(filepath.Join(outDir, "index.html"))
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	homeStr := string(homeContent)
	if !strings.Contains(homeStr, "Ollie") {
		t.Errorf("index.html missing 'Ollie'")
	}
	if !strings.Contains(homeStr, "Should I try this today?") {
		t.Errorf("index.html missing 'Should I try this today?' heading/prose")
	}

	// Verify static redirect stubs copied unchanged
	staticRedirect, err := os.ReadFile(filepath.Join(outDir, "docs", "index.html"))
	if err != nil {
		t.Fatalf("reading docs/index.html: %v", err)
	}
	if !strings.Contains(string(staticRedirect), "meta http-equiv=\"refresh\"") {
		t.Errorf("docs/index.html redirect stub not preserved")
	}
}

func TestRoutePath(t *testing.T) {
	tests := []struct {
		rel  string
		ext  string
		want string
	}{
		{"index.md", ".md", "index.html"},
		{"index.mdx", ".mdx", "index.html"},
		{"404.md", ".md", "404.html"},
		{"getting-started.md", ".md", "getting-started/index.html"},
		{"use-cases/hpa-autoscaling.md", ".md", "use-cases/hpa-autoscaling/index.html"},
		{"nested/index.md", ".md", "nested/index.html"},
		{"nested/404.md", ".md", "nested/404.html"},
	}

	for _, tc := range tests {
		got := filepath.ToSlash(routePath(tc.rel, tc.ext))
		if got != tc.want {
			t.Errorf("routePath(%q, %q) = %q, want %q", tc.rel, tc.ext, got, tc.want)
		}
	}
}

func TestParseFrontMatter(t *testing.T) {
	src := []byte("---\ntitle: Sample Title\nfoo: bar\n---\n# Heading\nBody text\n")
	meta, body, err := parseFrontMatter(src)
	if err != nil {
		t.Fatalf("parseFrontMatter failed: %v", err)
	}
	if meta.Title != "Sample Title" {
		t.Errorf("got title %q, want %q", meta.Title, "Sample Title")
	}
	if !strings.Contains(string(body), "# Heading") {
		t.Errorf("body missing heading: %s", string(body))
	}
}
