package server

import (
	"context"
	"net/http"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/httpx"
)

// Pinger is the readiness dependency. An interface rather than *pgxpool.Pool
// so health checks are testable without a database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// handleLive answers "is this process alive". It deliberately touches nothing
// external.
//
// This is the distinction that matters: if liveness checked the database, a
// database blip would fail liveness, the orchestrator would kill every
// instance, and they would restart into the same failing database. A brief
// dependency outage becomes a restart storm. Liveness says "restart me";
// readiness says "route around me".
func handleLive(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady answers "can this process serve traffic", which does require the
// database. A failure here takes the instance out of rotation without killing
// it.
func handleReady(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			httpx.WriteProblem(w, r, httpx.Errorf(
				http.StatusServiceUnavailable, "database unreachable"))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
