package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/auth"
	"github.com/Trones21/bluestaq-takehome/internal/config"
	"github.com/Trones21/bluestaq-takehome/internal/obs"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type stubPinger struct{ err error }

func (s stubPinger) Ping(context.Context) error { return s.err }

func testDeps(t *testing.T, db Pinger) Deps {
	t.Helper()
	return Deps{
		Config:  &config.Config{CORSOrigins: []string{"https://notes.example"}},
		Tokens:  auth.NewTokens([]byte(strings.Repeat("k", 32)), time.Hour),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Metrics: obs.NewMetrics(),
		DB:      db,
	}
}

func TestLivenessDoesNotDependOnDatabase(t *testing.T) {
	// The point of the liveness/readiness split: a database outage must not
	// fail liveness, or the orchestrator kills every instance and they restart
	// into the same failing database.
	d := testDeps(t, stubPinger{err: errors.New("connection refused")})
	r := AdminRouter(d)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness must stay 200 while the database is down, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness must fail while the database is down, got %d", rec.Code)
	}
}

func TestReadinessPassesWhenDatabaseIsUp(t *testing.T) {
	r := AdminRouter(testDeps(t, stubPinger{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

// TestMetricsLabelByRoutePatternNotPath pins the cardinality rule from SPEC 10.
//
// Prometheus client metrics are held in an in-process map keyed by label tuple,
// and because scraping is pull-based nothing is ever evicted. Labelling by raw
// path would add a permanent entry per note UUID ever requested. This test
// fails if someone swaps RoutePattern() for r.URL.Path.
func TestMetricsLabelByRoutePatternNotPath(t *testing.T) {
	m := obs.NewMetrics()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	r := chi.NewRouter()
	r.Use(obs.RequestID)
	r.Use(obs.Observe(m, log))
	r.Get("/v1/notes/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Three distinct IDs. If labelling used the raw path this produces three
	// series; with the route pattern it produces one.
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/notes/"+uuid.NewString(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d", i, rec.Code)
		}
	}

	series := collect(t, m.Registry(), "http_requests_total")
	if len(series) != 1 {
		t.Fatalf("want 1 series for 3 distinct note IDs, got %d -- labels are using the raw path", len(series))
	}
	if got := labelValue(series[0], "route"); got != "/v1/notes/{id}" {
		t.Fatalf("route label = %q, want the chi route pattern", got)
	}
	if got := series[0].GetCounter().GetValue(); got != 3 {
		t.Fatalf("counter = %v, want 3", got)
	}
}

// Unmatched requests collapse to a single label too, so a 404 scan across
// random paths cannot inflate cardinality either.
func TestUnmatchedRoutesDoNotCreateSeriesPerPath(t *testing.T) {
	m := obs.NewMetrics()
	r := chi.NewRouter()
	r.Use(obs.RequestID)
	r.Use(obs.Observe(m, slog.New(slog.NewJSONHandler(io.Discard, nil))))

	for _, p := range []string{"/nope/a", "/nope/b", "/nope/c"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	}

	for _, s := range collect(t, m.Registry(), "http_requests_total") {
		if v := labelValue(s, "route"); v != "unmatched" {
			t.Fatalf("unmatched request produced route label %q", v)
		}
	}
}

func TestErrorsAreProblemJSON(t *testing.T) {
	r := PublicRouter(testDeps(t, stubPinger{}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	for _, k := range []string{"type", "title", "status"} {
		if _, ok := body[k]; !ok {
			t.Errorf("problem body missing required member %q", k)
		}
	}
}

func TestCORSDefaultsToDeny(t *testing.T) {
	d := testDeps(t, stubPinger{})
	d.Config = &config.Config{} // no configured origins
	r := PublicRouter(d)

	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unconfigured service echoed an origin: %q", got)
	}
}

func TestCORSAllowsConfiguredOriginAndExposesETag(t *testing.T) {
	r := PublicRouter(testDeps(t, stubPinger{}))

	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	req.Header.Set("Origin", "https://notes.example")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://notes.example" {
		t.Fatalf("Allow-Origin = %q", got)
	}
	// If-Match round-trips through the client, so ETag must be readable by it.
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "ETag") {
		t.Fatalf("Expose-Headers = %q, must include ETag for optimistic concurrency", got)
	}
}

func TestRequestIDIsEchoedAndGeneratedWhenAbsent(t *testing.T) {
	r := PublicRouter(testDeps(t, stubPinger{}))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("no request ID generated")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("X-Request-Id", "caller-supplied")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "caller-supplied" {
		t.Fatalf("X-Request-Id = %q, want the caller's value preserved", got)
	}
}

func collect(t *testing.T, reg *prometheus.Registry, name string) []*dto.Metric {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()
		}
	}
	return nil
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}
