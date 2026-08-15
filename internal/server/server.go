// Package server assembles the HTTP routers and their middleware.
//
// Two listeners, deliberately: the public API, and an admin listener carrying
// /metrics, /healthz and /readyz. Metrics expose internal route structure,
// request volumes and error rates, none of which belong on a public port.
package server

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/Trones21/bluestaq-takehome/internal/auth"
	"github.com/Trones21/bluestaq-takehome/internal/config"
	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/Trones21/bluestaq-takehome/internal/notes"
	"github.com/Trones21/bluestaq-takehome/internal/obs"
	"github.com/Trones21/bluestaq-takehome/internal/teams"
	"github.com/Trones21/bluestaq-takehome/internal/users"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Deps is everything the routers need. Explicit rather than a service locator
// so the wiring is visible at the call site.
type Deps struct {
	Config  *config.Config
	Logger  *slog.Logger
	Metrics *obs.Metrics
	DB      Pinger
	Tokens  *auth.Tokens

	// Feature handlers. Nil-checked so router tests can build a partial
	// server without standing up every dependency.
	Users *users.Handler
	Notes *notes.Handler
	Teams *teams.Handler
}

// PublicRouter builds the API listener.
func PublicRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	// Order matters: request ID first so every later line can reference it,
	// recovery inside Observe so panics are still counted and logged as 500s.
	r.Use(obs.RequestID)
	r.Use(obs.Observe(d.Metrics, d.Logger))
	r.Use(obs.Recover)
	r.Use(cors(d.Config.CORSOrigins))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusNotFound, "no such endpoint"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, httpx.Errorf(
			http.StatusMethodNotAllowed, "method not allowed for this endpoint"))
	})

	r.Route("/v1", func(r chi.Router) {
		// Unauthenticated: obtaining a token.
		if d.Users != nil {
			r.Group(d.Users.PublicRoutes)
		}

		// Everything else requires one. Grouped rather than per-route so a new
		// endpoint is authenticated by default -- forgetting to add middleware
		// is a far likelier mistake than forgetting to remove it.
		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(d.Tokens))
			if d.Users != nil {
				r.Group(d.Users.AuthedRoutes)
			}
			if d.Notes != nil {
				r.Group(d.Notes.Routes)
			}
			if d.Teams != nil {
				r.Group(d.Teams.Routes)
			}
		})
	})

	return r
}

// AdminRouter builds the operator listener. Never expose this publicly.
func AdminRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(obs.Recover)

	r.Get("/healthz", handleLive)
	r.Get("/readyz", handleReady(d.DB))
	r.Handle("/metrics", promhttp.HandlerFor(
		d.Metrics.Registry(), promhttp.HandlerOpts{}))

	return r
}

// cors applies a strict allowlist. Default deny: with no configured origins
// the browser preflight simply fails, which is the correct posture for a
// service that has no browser client yet.
func cors(allowed []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && slices.Contains(allowed, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Headers",
					strings.Join([]string{"Authorization", "Content-Type", "If-Match", "X-Request-Id"}, ", "))
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				// If-Match round-trips, so the client must be able to read ETag.
				w.Header().Set("Access-Control-Expose-Headers", "ETag, X-Request-Id")
				w.Header().Set("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
