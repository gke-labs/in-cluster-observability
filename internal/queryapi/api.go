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

// Package queryapi serves the query server's Prometheus-compatible
// HTTP API (#94, #95): /api/v1/query and /api/v1/query_range
// evaluated by the stock PromQL engine over the fan-out queryable,
// plus /healthz/live and /healthz/ready.
//
// Responses use the standard Prometheus JSON envelope. When the
// fan-out skipped agents, the envelope additionally carries
// degraded=true + missingNodes (unknown fields are ignored by
// standard clients) and a matching entry in warnings.
package queryapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"

	"github.com/gke-labs/in-cluster-observability/internal/fanout"
)

// Config for New.
type Config struct {
	Queryable storage.Queryable
	// Ready gates /healthz/ready (true once ≥1 agent is discovered).
	Ready func() bool
	// Timeout is the overall per-query deadline. Defaults to 30s.
	Timeout time.Duration
	Logger  *slog.Logger
}

// API evaluates PromQL over the fan-out.
type API struct {
	engine    *promql.Engine
	queryable storage.Queryable
	ready     func() bool
	timeout   time.Duration
	logger    *slog.Logger
}

// New builds the API and its engine.
func New(cfg Config) *API {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Ready == nil {
		cfg.Ready = func() bool { return true }
	}
	eng := promql.NewEngine(promql.EngineOpts{
		Logger:     cfg.Logger.With("component", "promql"),
		MaxSamples: 50_000_000,
		Timeout:    cfg.Timeout,
		NoStepSubqueryIntervalFn: func(int64) int64 {
			return time.Minute.Milliseconds()
		},
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
	})
	return &API{
		engine:    eng,
		queryable: cfg.Queryable,
		ready:     cfg.Ready,
		timeout:   cfg.Timeout,
		logger:    cfg.Logger,
	}
}

// Routes mounts the API on a fresh mux.
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", a.handleQuery)
	mux.HandleFunc("/api/v1/query_range", a.handleQueryRange)
	mux.HandleFunc("/healthz/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/healthz/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !a.ready() {
			http.Error(w, "no agents discovered yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// InstantVector evaluates one instant query over the fan-out and
// returns the resulting vector (scalar results are rejected; the
// custom-metrics templates all aggregate to vectors). Degraded
// fan-outs still return data — the HPA gets a slightly-low number
// rather than no number (storage-and-query.md §5.3).
func (a *API) InstantVector(ctx context.Context, expr string, ts time.Time) (promql.Vector, error) {
	vec, _, err := a.InstantVectorDegraded(ctx, expr, ts)
	return vec, err
}

// InstantVectorDegraded additionally reports whether the fan-out
// skipped any agent (consumed by the metric stream service, #99).
func (a *API) InstantVectorDegraded(ctx context.Context, expr string, ts time.Time) (promql.Vector, bool, error) {
	ctx, stats := fanout.WithStats(ctx)
	q, err := a.engine.NewInstantQuery(ctx, a.queryable, nil, expr, ts)
	if err != nil {
		return nil, false, err
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return nil, false, res.Err
	}
	vec, err := res.Vector()
	return vec, stats.Degraded(), err
}

// envelope is the Prometheus API response shape plus the fan-out
// degradation extras.
type envelope struct {
	Status       string    `json:"status"`
	Data         *respData `json:"data,omitempty"`
	ErrorType    string    `json:"errorType,omitempty"`
	Error        string    `json:"error,omitempty"`
	Warnings     []string  `json:"warnings,omitempty"`
	Degraded     bool      `json:"degraded,omitempty"`
	MissingNodes []string  `json:"missingNodes,omitempty"`
}

type respData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func (a *API) handleQuery(w http.ResponseWriter, r *http.Request) {
	expr := r.FormValue("query")
	if expr == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing query parameter")
		return
	}
	ts, err := parseTime(r.FormValue("time"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid time: %v", err))
		return
	}

	ctx, stats := fanout.WithStats(r.Context())
	q, err := a.engine.NewInstantQuery(ctx, a.queryable, nil, expr, ts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer q.Close()
	a.execAndWrite(w, ctx, q, stats)
}

func (a *API) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	expr := r.FormValue("query")
	if expr == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing query parameter")
		return
	}
	start, err := parseTime(r.FormValue("start"), time.Time{})
	if err != nil || start.IsZero() {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid or missing start")
		return
	}
	end, err := parseTime(r.FormValue("end"), time.Time{})
	if err != nil || end.IsZero() {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid or missing end")
		return
	}
	step, err := parseDuration(r.FormValue("step"))
	if err != nil || step <= 0 {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid or missing step")
		return
	}
	if end.Before(start) {
		writeError(w, http.StatusBadRequest, "bad_data", "end before start")
		return
	}

	ctx, stats := fanout.WithStats(r.Context())
	q, err := a.engine.NewRangeQuery(ctx, a.queryable, nil, expr, start, end, step)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer q.Close()
	a.execAndWrite(w, ctx, q, stats)
}

func (a *API) execAndWrite(w http.ResponseWriter, ctx context.Context, q promql.Query, stats *fanout.Stats) {
	res := q.Exec(ctx)
	if res.Err != nil {
		writeError(w, http.StatusUnprocessableEntity, "execution", res.Err.Error())
		return
	}

	raw, rt, err := encodeValue(res.Value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	env := envelope{
		Status: "success",
		Data:   &respData{ResultType: rt, Result: raw},
	}
	for _, warn := range res.Warnings.AsErrors() {
		env.Warnings = append(env.Warnings, warn.Error())
	}
	if stats.Degraded() {
		env.Degraded = true
		env.MissingNodes = stats.Missing()
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("degraded: %d agent(s) missed the fan-out deadline: %v", len(env.MissingNodes), env.MissingNodes))
	}
	writeJSON(w, http.StatusOK, env)
}

// encodeValue renders a promql result in the API's wire shape by
// converting to the prometheus/common/model types, whose JSON
// marshaling is the API format.
func encodeValue(v parser.Value) (json.RawMessage, string, error) {
	switch val := v.(type) {
	case promql.Vector:
		vec := make(model.Vector, 0, len(val))
		for _, s := range val {
			m := model.Metric{}
			s.Metric.Range(func(l labels.Label) {
				m[model.LabelName(l.Name)] = model.LabelValue(l.Value)
			})
			vec = append(vec, &model.Sample{
				Metric:    m,
				Value:     model.SampleValue(s.F),
				Timestamp: model.Time(s.T),
			})
		}
		raw, err := json.Marshal(vec)
		return raw, "vector", err
	case promql.Matrix:
		mat := make(model.Matrix, 0, len(val))
		for _, s := range val {
			m := model.Metric{}
			s.Metric.Range(func(l labels.Label) {
				m[model.LabelName(l.Name)] = model.LabelValue(l.Value)
			})
			ss := &model.SampleStream{Metric: m}
			for _, p := range s.Floats {
				ss.Values = append(ss.Values, model.SamplePair{
					Timestamp: model.Time(p.T),
					Value:     model.SampleValue(p.F),
				})
			}
			mat = append(mat, ss)
		}
		raw, err := json.Marshal(mat)
		return raw, "matrix", err
	case promql.Scalar:
		raw, err := json.Marshal(model.Scalar{
			Value:     model.SampleValue(val.V),
			Timestamp: model.Time(val.T),
		})
		return raw, "scalar", err
	case promql.String:
		raw, err := json.Marshal(model.String{
			Value:     val.V,
			Timestamp: model.Time(val.T),
		})
		return raw, "string", err
	default:
		return nil, "", fmt.Errorf("unsupported result type %T", v)
	}
}

func writeError(w http.ResponseWriter, code int, errType, msg string) {
	writeJSON(w, code, envelope{Status: "error", ErrorType: errType, Error: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// parseTime accepts RFC3339 or Unix (fractional) seconds; empty
// yields def.
func parseTime(s string, def time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(frac*1e9)), nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// parseDuration accepts Prometheus duration strings ("30s", "1m") or
// bare (fractional) seconds.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := model.ParseDuration(s)
	return time.Duration(d), err
}
