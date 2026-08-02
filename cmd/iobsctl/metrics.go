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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func runMetrics(args []string) {
	fs := flag.NewFlagSet("metrics", flag.ExitOnError)
	g := addGlobalFlags(fs)
	timeArg := fs.String("time", "", "instant evaluation time (RFC3339 or unix seconds; default now)")
	startArg := fs.String("start", "", "range start (RFC3339, unix seconds, or relative like -15m); presence selects a range query")
	endArg := fs.String("end", "", "range end (default now)")
	stepArg := fs.String("step", "15s", "range step")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fatal("metrics needs exactly one PromQL argument")
	}
	expr := fs.Arg(0)

	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	local, stop, err := forward(ctx, g, 9095)
	if err != nil {
		fatal("%v", err)
	}
	defer stop()

	q := url.Values{"query": {expr}}
	path := "/api/v1/query"
	if *startArg != "" {
		path = "/api/v1/query_range"
		start, err := parseTimeArg(*startArg)
		if err != nil {
			fatal("--start: %v", err)
		}
		end := time.Now()
		if *endArg != "" {
			if end, err = parseTimeArg(*endArg); err != nil {
				fatal("--end: %v", err)
			}
		}
		q.Set("start", strconv.FormatInt(start.Unix(), 10))
		q.Set("end", strconv.FormatInt(end.Unix(), 10))
		q.Set("step", *stepArg)
	} else if *timeArg != "" {
		ts, err := parseTimeArg(*timeArg)
		if err != nil {
			fatal("--time: %v", err)
		}
		q.Set("time", strconv.FormatInt(ts.Unix(), 10))
	}

	tlsConf, err := queryTLSConfig(ctx, g)
	if err != nil {
		fatal("%v", err)
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}}
	u := fmt.Sprintf("https://127.0.0.1:%d%s?%s", local, path, q.Encode())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		fatal("query: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var env struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
		Warnings     []string `json:"warnings"`
		Degraded     bool     `json:"degraded"`
		MissingNodes []string `json:"missingNodes"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		fatal("decoding response (%d): %v\n%s", resp.StatusCode, err, body)
	}
	if env.Status != "success" {
		fatal("query failed: %s", env.Error)
	}

	if g.output == "json" {
		os.Stdout.Write(body)
		fmt.Println()
		return
	}
	for _, w := range env.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	switch env.Data.ResultType {
	case "vector":
		var result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(env.Data.Result, &result); err != nil {
			fatal("decoding vector: %v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "METRIC\tVALUE\tTIMESTAMP")
		for _, s := range result {
			fmt.Fprintf(tw, "%s\t%v\t%s\n", formatMetric(s.Metric), s.Value[1], formatUnix(s.Value[0]))
		}
		tw.Flush()
	case "matrix":
		var result []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		}
		if err := json.Unmarshal(env.Data.Result, &result); err != nil {
			fatal("decoding matrix: %v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "METRIC\tTIMESTAMP\tVALUE")
		for _, s := range result {
			name := formatMetric(s.Metric)
			for _, v := range s.Values {
				fmt.Fprintf(tw, "%s\t%s\t%v\n", name, formatUnix(v[0]), v[1])
			}
		}
		tw.Flush()
	default:
		fmt.Printf("%s\n", env.Data.Result)
	}
}

func formatMetric(m map[string]string) string {
	name := m["__name__"]
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "__name__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, m[k]))
	}
	if len(parts) == 0 {
		return name
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func formatUnix(v any) string {
	f, ok := v.(float64)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return time.Unix(int64(f), 0).UTC().Format(time.RFC3339)
}

// parseTimeArg accepts RFC3339, unix seconds, or a relative
// duration like "-15m" (relative to now).
func parseTimeArg(s string) (time.Time, error) {
	if strings.HasPrefix(s, "-") {
		d, err := time.ParseDuration(s)
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().Add(d), nil
	}
	if sec, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(int64(sec), 0), nil
	}
	return time.Parse(time.RFC3339, s)
}
