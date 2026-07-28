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

package otlpreceiver

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"

	colllogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// registerHTTP wires the OTLP/HTTP collector paths per the OTLP spec.
// Each endpoint accepts both application/x-protobuf and application/json.
func registerHTTP(mux *http.ServeMux, h Handler) {
	mux.Handle("POST /v1/metrics", &httpMetrics{h: h})
	mux.Handle("POST /v1/traces", &httpTraces{h: h})
	mux.Handle("POST /v1/logs", &httpLogs{h: h})
}

type httpMetrics struct{ h Handler }

func (x *httpMetrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := &collmetricspb.ExportMetricsServiceRequest{}
	if !readBody(w, r, req) {
		return
	}
	if err := x.h.OnMetrics(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeBody(w, r, &collmetricspb.ExportMetricsServiceResponse{})
}

type httpTraces struct{ h Handler }

func (x *httpTraces) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := &colltracepb.ExportTraceServiceRequest{}
	if !readBody(w, r, req) {
		return
	}
	if err := x.h.OnTraces(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeBody(w, r, &colltracepb.ExportTraceServiceResponse{})
}

type httpLogs struct{ h Handler }

func (x *httpLogs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := &colllogspb.ExportLogsServiceRequest{}
	if !readBody(w, r, req) {
		return
	}
	if err := x.h.OnLogs(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeBody(w, r, &colllogspb.ExportLogsServiceResponse{})
}

func readBody(w http.ResponseWriter, r *http.Request, msg proto.Message) bool {
	defer r.Body.Close()

	// OTLP/HTTP allows gzip-compressed bodies (#154 — an OBI image
	// that enables exporter compression must not 400).
	var reader io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		defer gz.Close()
		reader = gz
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}

	// Parse the media type properly: "application/json; charset=utf-8"
	// must route to the JSON decoder, not fall through to protobuf
	// (#154). An absent/unparseable Content-Type defaults to protobuf,
	// matching the OTLP spec's primary encoding.
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType == "application/json" {
		if err := protojson.Unmarshal(body, msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
	} else {
		if err := proto.Unmarshal(body, msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
	}
	return true
}

func writeBody(w http.ResponseWriter, r *http.Request, msg proto.Message) {
	accept := r.Header.Get("Accept")
	var (
		body []byte
		err  error
	)
	if accept == "application/json" {
		body, err = protojson.Marshal(msg)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
		}
	} else {
		body, err = proto.Marshal(msg)
		if err == nil {
			w.Header().Set("Content-Type", "application/x-protobuf")
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}
