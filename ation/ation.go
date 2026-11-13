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
	"bytes"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"gopkg.in/yaml.v3"
)

// Config specifies the source content, static asset directory, and output directory for the build.
type Config struct {
	ContentDir string
	StaticDir  string
	OutputDir  string
}

// FrontMatter holds metadata extracted from YAML frontmatter in Markdown/MDX files.
type FrontMatter struct {
	Title string `yaml:"title"`
}

// Build reads docs content from cfg.ContentDir, copies static files from cfg.StaticDir (if set),
// renders Markdown/MDX pages to HTML, and writes the output site to cfg.OutputDir.
func Build(cfg Config) error {
	if cfg.ContentDir == "" || cfg.OutputDir == "" {
		return fmt.Errorf("content and output directories are required")
	}
	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}
	if cfg.StaticDir != "" {
		if err := copyStaticFiles(cfg.StaticDir, cfg.OutputDir); err != nil {
			return fmt.Errorf("copying static files: %w", err)
		}
	}

	md := goldmark.New(goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()))

	return filepath.Walk(cfg.ContentDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		ext := filepath.Ext(path)
		if ext != ".md" && ext != ".mdx" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		meta, body, err := parseFrontMatter(data)
		if err != nil {
			return fmt.Errorf("parsing frontmatter in %s: %w", path, err)
		}

		var buf bytes.Buffer
		if err := md.Convert(body, &buf); err != nil {
			return fmt.Errorf("rendering markdown in %s: %w", path, err)
		}

		rel, err := filepath.Rel(cfg.ContentDir, path)
		if err != nil {
			return err
		}

		destPath := filepath.Join(cfg.OutputDir, routePath(rel, ext))
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("creating dir for %s: %w", destPath, err)
		}

		pageHTML := renderPage(meta.Title, buf.String())
		return os.WriteFile(destPath, []byte(pageHTML), 0644)
	})
}

func routePath(rel, ext string) string {
	noExt := strings.TrimSuffix(filepath.ToSlash(rel), ext)
	switch {
	case noExt == "index":
		return "index.html"
	case noExt == "404":
		return "404.html"
	case filepath.Base(noExt) == "index":
		return filepath.Join(filepath.Dir(noExt), "index.html")
	case filepath.Base(noExt) == "404":
		return filepath.Join(filepath.Dir(noExt), "404.html")
	default:
		return filepath.Join(noExt, "index.html")
	}
}

func parseFrontMatter(data []byte) (FrontMatter, []byte, error) {
	var meta FrontMatter
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return meta, data, nil
	}
	idx := bytes.IndexByte(data, '\n')
	rest := data[idx+1:]
	end := bytes.Index(rest, []byte("\n---"))
	if end == -1 {
		return meta, data, nil
	}
	yamlBytes := rest[:end]
	remainder := rest[end+len("\n---"):]
	var body []byte
	if nl := bytes.IndexByte(remainder, '\n'); nl != -1 {
		body = remainder[nl+1:]
	}
	if err := yaml.Unmarshal(yamlBytes, &meta); err != nil {
		return meta, body, err
	}
	return meta, body, nil
}

func renderPage(title, bodyHTML string) string {
	return "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n  <meta charset=\"utf-8\">\n  <title>" +
		html.EscapeString(title) +
		"</title>\n</head>\n<body>\n" +
		bodyHTML +
		"</body>\n</html>\n"
}

func copyStaticFiles(srcDir, outDir string) error {
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		return nil
	}
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(outDir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, info.Mode())
	})
}
