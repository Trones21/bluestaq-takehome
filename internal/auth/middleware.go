package auth

import (
	"net/http"
	"strings"

	"github.com/Trones21/bluestaq-takehome/internal/httpx"
)

// Middleware authenticates the bearer token and puts the caller on the
// context. Routes mounted behind it can rely on httpx.MustUserID.
//
// It verifies the token only -- it does not load the user. Checking that the
// account still exists on every request would add a database round trip to
// every endpoint, and each of those endpoints already fails naturally against
// a deleted user (their notes are unreachable, their grants removed). The
// tradeoff is that a token outlives account deletion until it expires, which
// is the same revocation gap stateless auth has generally.
func Middleware(t *Tokens) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				unauthorized(w, r, "a bearer token is required")
				return
			}
			userID, err := t.Parse(raw)
			if err != nil {
				// The parse error is deliberately not echoed: distinguishing
				// "expired" from "bad signature" tells an attacker which half
				// of a forgery attempt worked.
				unauthorized(w, r, "invalid or expired token")
				return
			}
			next.ServeHTTP(w, r.WithContext(httpx.WithUserID(r.Context(), userID)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, r *http.Request, detail string) {
	// RFC 9110 requires a challenge on a 401.
	w.Header().Set("WWW-Authenticate", `Bearer realm="notes"`)
	httpx.WriteProblem(w, r, httpx.Errorf(http.StatusUnauthorized, "%s", detail))
}
