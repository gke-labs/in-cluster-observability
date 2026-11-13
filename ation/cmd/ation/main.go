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
	"flag"
	"fmt"
	"os"

	"github.com/gke-labs/in-cluster-observability/ation"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ation: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("ation", flag.ContinueOnError)
	var cfg ation.Config
	fs.StringVar(&cfg.ContentDir, "content", "", "markdown content directory")
	fs.StringVar(&cfg.StaticDir, "static", "", "static assets directory")
	fs.StringVar(&cfg.OutputDir, "out", "", "output directory")

	if len(args) > 0 && args[0] == "build" {
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if pos := fs.Args(); len(pos) > 0 {
		if cfg.ContentDir == "" && len(pos) > 0 {
			cfg.ContentDir = pos[0]
		}
		if cfg.OutputDir == "" && len(pos) > 1 {
			cfg.OutputDir = pos[1]
		}
		if cfg.StaticDir == "" && len(pos) > 2 {
			cfg.StaticDir = pos[2]
		}
	}
	if cfg.ContentDir == "" || cfg.OutputDir == "" {
		return fmt.Errorf("usage: ation [build] -content <dir> -out <dir> [-static <dir>]")
	}
	return ation.Build(cfg)
}
