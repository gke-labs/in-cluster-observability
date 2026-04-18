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

package obs

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"log"
	"os"
)

type Attribute = attribute.KeyValue

func String(k, v string) Attribute {
	return attribute.String(k, v)
}

func Int(k string, v int) Attribute {
	return attribute.Int(k, v)
}

func Int64(k string, v int64) Attribute {
	return attribute.Int64(k, v)
}

func Bool(k string, v bool) Attribute {
	return attribute.Bool(k, v)
}

func Float64(k string, v float64) Attribute {
	return attribute.Float64(k, v)
}

type Span struct {
	logr.Logger
	span trace.Span
}

func (s Span) End(options ...trace.SpanEndOption) {
	if s.span != nil {
		s.span.End(options...)
	}
}

func DefaultLogger() logr.Logger {
	stdr.SetVerbosity(0)
	return stdr.New(log.New(os.Stderr, "", log.LstdFlags))
}

func FromContext(ctx context.Context) logr.Logger {
	l, err := logr.FromContext(ctx)
	if err == nil {
		return l
	}
	return DefaultLogger()
}

func Start(ctx context.Context, spanName string, attributes ...Attribute) (context.Context, Span) {
	tracer := otel.Tracer("obs")
	ctx, otelSpan := tracer.Start(ctx, spanName, trace.WithAttributes(attributes...))

	log := FromContext(ctx)

	var kvs []any
	for _, attr := range attributes {
		kvs = append(kvs, string(attr.Key), attr.Value.AsInterface())
	}

	if len(kvs) > 0 {
		log = log.WithValues(kvs...)
	}

	ctx = logr.NewContext(ctx, log)

	return ctx, Span{
		Logger: log,
		span:   otelSpan,
	}
}
