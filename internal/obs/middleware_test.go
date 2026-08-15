package obs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue reads a single labelled counter out of the registry.
func counterValue(t *testing.T, c *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestTimeoutCancelsTheRequestContext(t *testing.T) {
	// The behaviour that matters: a handler blocked on something context aware
	// -- in production, waiting for a pgxpool connection -- gets unblocked.
	// Without this the server's WriteTimeout closes the socket while the
	// goroutine keeps waiting, and the queue of waiters is unbounded.
	m := NewMetrics()

	var gotErr error
	h := Timeout(m, 20*time.Millisecond)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
				gotErr = r.Context().Err()
			case <-time.After(2 * time.Second):
				t.Error("handler was never cancelled")
			}
		}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/notes", nil))

	if !errors.Is(gotErr, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", gotErr)
	}
}

func TestTimeoutCountsByRoutePatternNotRawPath(t *testing.T) {
	// Same cardinality rule as the rest of the metrics: a note UUID in the
	// path must never become a permanent label value.
	m := NewMetrics()

	r := chi.NewRouter()
	r.Use(Timeout(m, 10*time.Millisecond))
	r.Get("/v1/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/notes/3f2b1c40-0000-0000-0000-000000000000", nil))

	if got := counterValue(t, m.Timeouts, http.MethodGet, "/v1/notes/{id}"); got != 1 {
		t.Fatalf("want one timeout counted against the route pattern, got %v", got)
	}
}

func TestTimeoutDoesNotFireOnFastRequests(t *testing.T) {
	m := NewMetrics()

	h := Timeout(m, time.Minute)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/notes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := counterValue(t, m.Timeouts, http.MethodGet, "unmatched"); got != 0 {
		t.Fatalf("want no timeouts counted, got %v", got)
	}
}

func TestTimeoutIsInertWhenUnset(t *testing.T) {
	// Guards a hand-built Config in tests: zero must mean "no deadline", not
	// "expire immediately".
	m := NewMetrics()

	called := false
	h := Timeout(m, 0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if _, ok := r.Context().Deadline(); ok {
			t.Error("no deadline should have been set")
		}
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/notes", nil))

	if !called {
		t.Fatal("handler was not reached")
	}
}
