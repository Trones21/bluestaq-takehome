package obs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// RequestID assigns each request an ID, echoes it in the response, and puts it
// on the context so every log line from one request correlates.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), id)))
	})
}

// Timeout gives every request a deadline.
//
// This is the piece that makes the bounded pool actually shed load. MaxConns
// caps concurrent queries and parks the rest, but a parked goroutine waits on
// its context, and without a deadline that wait has no upper bound -- the
// server's WriteTimeout closes the socket without cancelling the request
// context, so the handler keeps waiting for a client that is already gone.
// Under sustained overload that queue grows until the process does.
//
// It cancels rather than pre-empts: the deadline unblocks anything context
// aware (pgx, the S3 client) and those return context.DeadlineExceeded, which
// httpx.WriteProblem renders as a 503. A handler that ignored its context
// entirely would still run to completion -- accepted, because the alternative
// (http.TimeoutHandler) writes its own plain-text body and would put one
// response outside the problem+json contract every other error obeys.
func Timeout(m *Metrics, d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// A zero duration means an unset field on a hand-built Config, not "no
		// time at all" -- config.Load rejects a non-positive REQUEST_TIMEOUT,
		// so this can only be a caller assembling Deps directly. Passing
		// through beats expiring every request instantly.
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)

			// Checked after the handler returns, when chi has resolved the
			// route pattern. Same cardinality rule as everywhere else: the
			// pattern, never the raw path.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				route := "unmatched"
				if rc := chi.RouteContext(ctx); rc != nil && rc.RoutePattern() != "" {
					route = rc.RoutePattern()
				}
				m.Timeouts.WithLabelValues(r.Method, route).Inc()
			}
		})
	}
}

// statusRecorder captures the status code and byte count, which the
// ResponseWriter does not expose after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Observe logs and measures every request.
//
// The cardinality rule this enforces: metric labels use the chi *route
// pattern* ("/v1/notes/{id}"), while the log line carries the raw path.
// Prometheus is pull-based, so a client-side child metric lives for the life
// of the process -- labelling by raw path would add a permanent, never-evicted
// entry per note UUID ever requested. Logs have the opposite shape:
// write-and-forget, indexed elsewhere, cardinality is free. So each gets the
// form that is cheap for it. See SPEC 10.
func Observe(m *Metrics, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.InFlight.Inc()
			defer m.InFlight.Dec()

			rec := &statusRecorder{ResponseWriter: w}

			l := log.With(slog.String("request_id", httpx.RequestID(r)))
			r = r.WithContext(httpx.WithLogger(r.Context(), l))

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			elapsed := time.Since(start)

			// Resolved after ServeHTTP: chi fills the route context during
			// routing. Unmatched requests report "unmatched" rather than the
			// raw path, so 404 scans cannot inflate cardinality either.
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}

			m.Requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
			m.Duration.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())

			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path), // raw path: cheap here
				slog.String("route", route),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("duration", elapsed),
			}
			if uid, ok := httpx.UserID(r.Context()); ok {
				attrs = append(attrs, slog.String("user_id", uid.String()))
			}

			// Note titles, bodies, emails, tokens and presigned URLs are never
			// logged. Logging is by explicit field, never by struct dump, so
			// this holds by construction rather than by discipline.
			switch {
			case rec.status >= 500:
				l.Error("request", attrs...)
			case rec.status >= 400:
				l.Warn("request", attrs...)
			default:
				l.Info("request", attrs...)
			}
		})
	}
}

// Recover turns a panic into a 500 instead of a dropped connection, and logs
// it with the request ID so it can be tied to the rest of the request's trail.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				httpx.Logger(r).Error("panic recovered", slog.Any("panic", rec))
				httpx.WriteProblem(w, r, httpx.Errorf(
					http.StatusInternalServerError, "an internal error occurred"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
