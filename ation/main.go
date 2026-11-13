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

// Package main provides the ation command-line tool, which builds static
// sites by running builder container images offline.
//
// Builder Image Contract:
//   - The container runs without network access (--network none).
//   - A copy of the site source directory is provided at /src.
//   - The image's entrypoint builds the site and leaves the result in /out.
//   - ation copies the result from /out to the destination output directory,
//     clearing any existing contents and ensuring files are owned by the
//     invoking user on the host.
package main

import (
	"archive/tar"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

// DefaultExcludes lists generic directories omitted when streaming
// the site source into the builder container.
var DefaultExcludes = []string{".git"}

// Config holds the parameters for building a site.
type Config struct {
	Src      string
	Out      string
	Image    string
	Excludes []string
}

// Runner defines the interface for running a builder container.
type Runner interface {
	Run(ctx context.Context, cfg Config) error
}

// DockerRunner runs the builder container using a container engine (e.g. docker).
type DockerRunner struct {
	Engine string
	Stdout io.Writer
	Stderr io.Writer
}

func validatePaths(src, out string) (string, string, error) {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return "", "", fmt.Errorf("resolving source path: %w", err)
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return "", "", fmt.Errorf("resolving output path: %w", err)
	}

	info, err := os.Stat(srcAbs)
	if err != nil {
		return "", "", fmt.Errorf("source directory %q: %w", src, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("source directory %q is not a directory", src)
	}

	rel, err := filepath.Rel(outAbs, srcAbs)
	if err == nil && !strings.HasPrefix(rel, "..") {
		if rel == "." {
			return "", "", fmt.Errorf("output directory %q cannot be the source directory %q", out, src)
		}
		return "", "", fmt.Errorf("output directory %q cannot contain source directory %q", out, src)
	}

	return srcAbs, outAbs, nil
}

func computeExcludes(srcAbs, outAbs string, userExcludes []string) []string {
	var excludes []string
	excludes = append(excludes, DefaultExcludes...)
	excludes = append(excludes, userExcludes...)

	// If output directory lies inside the source directory, automatically exclude it.
	relOut, err := filepath.Rel(srcAbs, outAbs)
	if err == nil && !strings.HasPrefix(relOut, "..") && relOut != "." {
		excludes = append(excludes, filepath.ToSlash(relOut))
	}

	return excludes
}

func matchesExclude(relPath string, exclude string) bool {
	cleanRel := filepath.ToSlash(filepath.Clean(relPath))
	cleanExcl := filepath.ToSlash(filepath.Clean(exclude))
	return cleanRel == cleanExcl || strings.HasPrefix(cleanRel, cleanExcl+"/")
}

// writeTarStream walks srcDir and streams a tar archive to w, skipping excluded paths.
// Files in the archive are placed under a "src/" root prefix so docker cp extracts to /src.
func writeTarStream(srcDir string, excludes []string, w io.Writer) (retErr error) {
	tw := tar.NewWriter(w)
	defer func() {
		if err := tw.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("closing tar writer: %w", err)
		}
	}()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		for _, excl := range excludes {
			if matchesExclude(rel, excl) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("reading symlink %q: %w", path, err)
			}
		}

		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("creating tar header for %q: %w", path, err)
		}

		header.Name = filepath.ToSlash(filepath.Join("src", rel))
		if info.IsDir() {
			header.Name += "/"
		}

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("writing tar header for %q: %w", path, err)
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("opening file %q: %w", path, err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				if closeErr := f.Close(); closeErr != nil {
					return fmt.Errorf("writing file %q to tar: %w (close error: %v)", path, err, closeErr)
				}
				return fmt.Errorf("writing file %q to tar: %w", path, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("closing file %q: %w", path, err)
			}
		}

		return nil
	})
}

// copyIn streams the source directory archive into the container.
func (r *DockerRunner) copyIn(ctx context.Context, engine, containerID, srcAbs string, excludes []string) error {
	logger := klog.FromContext(ctx)
	logger.Info("streaming source to container", "src", srcAbs, "container", containerID)

	pr, pw := io.Pipe()
	cpCmd := exec.CommandContext(ctx, engine, "cp", "-", containerID+":/")
	cpCmd.Stdin = pr

	tarErrCh := make(chan error, 1)
	go func() {
		err := writeTarStream(srcAbs, excludes, pw)
		_ = pw.CloseWithError(err)
		tarErrCh <- err
	}()

	cpOut, cpErr := cpCmd.CombinedOutput()
	// Unblock the writing goroutine if the command exited early and stopped reading.
	_ = pr.CloseWithError(cpErr)

	tarErr := <-tarErrCh
	if cpErr != nil {
		return fmt.Errorf("copying source to container: %w (%s)", cpErr, strings.TrimSpace(string(cpOut)))
	}
	if tarErr != nil && !errors.Is(tarErr, io.ErrClosedPipe) {
		return fmt.Errorf("creating source tar stream: %w", tarErr)
	}

	return nil
}

func (r *DockerRunner) Run(ctx context.Context, cfg Config) error {
	logger := klog.FromContext(ctx)

	engine := r.Engine
	if engine == "" {
		if env := os.Getenv("CONTAINER_ENGINE"); env != "" {
			engine = env
		} else {
			engine = "docker"
		}
	}

	srcAbs, outAbs, err := validatePaths(cfg.Src, cfg.Out)
	if err != nil {
		return err
	}

	logger.Info("clearing output directory", "out", outAbs)
	if err := os.RemoveAll(outAbs); err != nil {
		return fmt.Errorf("clearing output directory %q: %w", cfg.Out, err)
	}
	if err := os.MkdirAll(outAbs, 0755); err != nil {
		return fmt.Errorf("creating output directory %q: %w", cfg.Out, err)
	}

	logger.Info("creating builder container", "image", cfg.Image)
	createCmd := exec.CommandContext(ctx, engine, "create", "--network", "none", cfg.Image)
	createOut, err := createCmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("creating container: %w (%s)", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("creating container: %w", err)
	}
	containerID := strings.TrimSpace(string(createOut))

	defer func() {
		if out, err := exec.Command(engine, "rm", "-f", containerID).CombinedOutput(); err != nil {
			logger.Error(err, "failed to remove container", "container", containerID, "output", strings.TrimSpace(string(out)))
		}
	}()

	allExcludes := computeExcludes(srcAbs, outAbs, cfg.Excludes)
	if err := r.copyIn(ctx, engine, containerID, srcAbs, allExcludes); err != nil {
		return err
	}

	logger.Info("running build in container", "container", containerID)
	startCmd := exec.CommandContext(ctx, engine, "start", "-a", containerID)
	startCmd.Stdout = r.Stdout
	startCmd.Stderr = r.Stderr
	if err := startCmd.Run(); err != nil {
		return fmt.Errorf("running build in container: %w", err)
	}

	logger.Info("copying build output from container", "out", outAbs, "container", containerID)
	cpOutCmd := exec.CommandContext(ctx, engine, "cp", containerID+":/out/.", outAbs)
	if cpOutOut, err := cpOutCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copying output from container: %w (%s)", err, strings.TrimSpace(string(cpOutOut)))
	}

	return nil
}

func parseArgs(args []string, errOut io.Writer) (Config, error) {
	if len(args) == 0 {
		return Config{}, errors.New("expected 'build' subcommand")
	}
	if args[0] != "build" {
		return Config{}, fmt.Errorf("unknown command %q (expected 'build')", args[0])
	}

	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	if errOut != nil {
		fs.SetOutput(errOut)
	}
	fs.Usage = func() {
		var b strings.Builder
		b.WriteString("Usage: ation build -src <src-dir> -out <out-dir> -image <builder-image> [-exclude <comma-separated>]\n\n")
		b.WriteString("Builder image contract:\n")
		b.WriteString("  - A copy of the site source directory arrives in /src\n")
		b.WriteString("  - Image entrypoint builds the site offline (no network access)\n")
		b.WriteString("  - Result is read from /out and copied to -out (clearing any existing contents)\n\n")
		b.WriteString("Flags:\n")
		if _, err := io.WriteString(fs.Output(), b.String()); err != nil {
			klog.Errorf("failed to write usage: %v", err)
		}
		fs.PrintDefaults()
	}

	var cfg Config
	var excludeFlag string
	fs.StringVar(&cfg.Src, "src", "", "path to site source directory")
	fs.StringVar(&cfg.Out, "out", "", "path to output directory (cleared before build)")
	fs.StringVar(&cfg.Image, "image", "", "builder image name")
	fs.StringVar(&excludeFlag, "exclude", "", "comma-separated list of additional paths to exclude from source")

	if err := fs.Parse(args[1:]); err != nil {
		return Config{}, err
	}

	if cfg.Src == "" || cfg.Out == "" || cfg.Image == "" {
		fs.Usage()
		return Config{}, errors.New("missing required flags: -src, -out, and -image must all be specified")
	}

	if excludeFlag != "" {
		for _, part := range strings.Split(excludeFlag, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				cfg.Excludes = append(cfg.Excludes, part)
			}
		}
	}

	return cfg, nil
}

func run(ctx context.Context, args []string, runner Runner, errOut io.Writer) error {
	cfg, err := parseArgs(args, errOut)
	if err != nil {
		return err
	}
	return runner.Run(ctx, cfg)
}

func main() {
	ctx := context.Background()
	logger := klog.FromContext(ctx)

	runner := &DockerRunner{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	if err := run(ctx, os.Args[1:], runner, os.Stderr); err != nil {
		logger.Error(err, "ation build failed")
		os.Exit(1)
	}
}
