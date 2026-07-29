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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	streamv1 "github.com/gke-labs/in-cluster-observability/pkg/stream/pb/stream/v1"
)

func runSpans(args []string) {
	fs := flag.NewFlagSet("spans", flag.ExitOnError)
	g := addGlobalFlags(fs)
	filter := fs.String("filter", "", "CEL filter over span (OTLP Span) + resource (map<string,string>)")
	maxSpans := fs.Int("max", 0, "exit after this many spans (0 = stream until timeout/interrupt)")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	local, stop, err := forward(ctx, g, 9096)
	if err != nil {
		fatal("%v", err)
	}
	defer stop()

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", local),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial: %v", err)
	}
	defer conn.Close()

	stream, err := streamv1.NewStreamServiceClient(conn).SubscribeSpans(ctx,
		&streamv1.SubscribeSpansRequest{CelFilter: *filter})
	if err != nil {
		fatal("subscribe: %v", err)
	}
	fmt.Fprintln(os.Stderr, "subscribed; streaming spans (Ctrl-C to stop)")

	count := 0
	for {
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return
			}
			fatal("stream: %v", err)
		}
		var span tracepb.Span
		if err := proto.Unmarshal(ev.GetSpan(), &span); err != nil {
			fmt.Fprintf(os.Stderr, "warning: undecodable span: %v\n", err)
			continue
		}
		if ev.GetGap() > 0 {
			fmt.Fprintf(os.Stderr, "warning: %d span(s) dropped (slow consumer)\n", ev.GetGap())
		}
		printSpan(g.output, &span, ev.GetResource())
		count++
		if *maxSpans > 0 && count >= *maxSpans {
			return
		}
	}
}

func printSpan(output string, span *tracepb.Span, resource map[string]string) {
	if output == "json" {
		raw, _ := protojson.Marshal(span)
		out, _ := json.Marshal(map[string]any{
			"resource": resource,
			"span":     json.RawMessage(raw),
		})
		fmt.Println(string(out))
		return
	}
	start := time.Unix(0, int64(span.GetStartTimeUnixNano())).UTC()               //nolint:gosec // post-epoch
	dur := time.Duration(span.GetEndTimeUnixNano() - span.GetStartTimeUnixNano()) //nolint:gosec
	where := resource["k8s.namespace.name"]
	if pod := resource["k8s.pod.name"]; pod != "" {
		where += "/" + pod
	}
	fmt.Printf("%s  %-10s  %-40q  %s\n",
		start.Format("15:04:05.000"), dur, span.GetName(), where)
}
