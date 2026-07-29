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

// Package streamsvc implements proto/stream/v1 (#99, ADR-0026 §7):
// the agent's node-local span subscription service and the query
// server's cluster-wide multiplexer + metric streamer. CEL filters
// compile against the OTLP Span proto so field paths match what
// OTel users already know, and evaluate agent-side so non-matching
// spans never cross the network.
package streamsvc

import (
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// SpanFilter reports whether a span (with its resource attributes)
// matches a compiled CEL expression.
type SpanFilter func(span *tracepb.Span, resource map[string]string) (bool, error)

var (
	celEnvOnce sync.Once
	celEnv     *cel.Env
	celEnvErr  error
)

func spanEnv() (*cel.Env, error) {
	celEnvOnce.Do(func() {
		celEnv, celEnvErr = cel.NewEnv(
			cel.Types(&tracepb.Span{}),
			cel.Variable("span", cel.ObjectType("opentelemetry.proto.trace.v1.Span")),
			cel.Variable("resource", cel.MapType(cel.StringType, cel.StringType)),
		)
	})
	return celEnv, celEnvErr
}

// CompileFilter builds a SpanFilter from a CEL expression. Empty
// expressions match everything. The program is thread-safe and
// reusable across a stream's lifetime.
func CompileFilter(expr string) (SpanFilter, error) {
	if expr == "" {
		return func(*tracepb.Span, map[string]string) (bool, error) { return true, nil }, nil
	}
	env, err := spanEnv()
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, fmt.Errorf("compiling filter: %w", iss.Err())
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("filter must evaluate to bool, got %s", ast.OutputType())
	}
	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("building program: %w", err)
	}
	return func(span *tracepb.Span, resource map[string]string) (bool, error) {
		if resource == nil {
			resource = map[string]string{}
		}
		out, _, err := prg.Eval(map[string]any{
			"span":     span,
			"resource": resource,
		})
		if err != nil {
			return false, err
		}
		b, ok := out.Value().(bool)
		return ok && b, nil
	}, nil
}
