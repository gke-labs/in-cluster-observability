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

package parser

import (
	"strings"
)

type Query struct {
	BodyContains []string
	Attributes   map[string]string // resolved keys
}

// Map well-known aliases to OTLP attributes
var aliases = map[string]string{
	"namespace": "k8s.namespace.name",
	"pod":       "k8s.pod.name",
	"container": "k8s.container.name",
	"service":   "service.name",
	"severity":  "SeverityText",
}

func Parse(q string) *Query {
	query := &Query{
		BodyContains: []string{},
		Attributes:   make(map[string]string),
	}

	tokens := tokenize(q)

	for _, token := range tokens {
		if strings.Contains(token, "=") {
			parts := strings.SplitN(token, "=", 2)
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])

			// Strip outer quotes if present in value
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}

			// Resolve alias
			resolvedKey := key
			if realKey, ok := aliases[key]; ok {
				resolvedKey = realKey
			}

			query.Attributes[resolvedKey] = val
		} else {
			// Bare term. Strip outer quotes if present
			val := token
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
			if val != "" {
				query.BodyContains = append(query.BodyContains, val)
			}
		}
	}

	return query
}

// tokenize splits a query string into tokens, respecting double quotes.
func tokenize(q string) []string {
	var tokens []string
	var current strings.Builder
	inQuotes := false

	runes := []rune(q)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '"' {
			inQuotes = !inQuotes
			current.WriteRune(r)
		} else if (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inQuotes {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		} else {
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}
