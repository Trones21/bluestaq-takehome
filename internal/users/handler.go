// Package users implements the account and authentication endpoints.
package users

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/auth"
	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	Store  *store.Store
	Tokens *auth.Tokens
	Now    func() time.Time
}

func New(s *store.Store, t *auth.Tokens) *Handler {
	return &Handler{Store: s, Tokens: t, Now: time.Now}
}

// PublicRoutes are reachable without a token.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/auth/register", h.register)
	r.Post("/auth/login", h.login)
}

// AuthedRoutes sit behind the auth middleware.
func (h *Handler) AuthedRoutes(r chi.Router) {
	r.Get("/users/me", h.me)
	r.Patch("/users/me", h.updateMe)
	r.Post("/users/me/password", h.changePassword)
	r.Delete("/users/me", h.deleteMe)
	r.Get("/users", h.lookup)
}

// User is the wire representation. Built by hand rather than tagging the
// database model, so a column added to `users` can never leak by default --
// password_hash most of all.
type User struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	Deleted     bool      `json:"deleted,omitempty"`
}

func toUser(u sqlcgen.User) User {
	return User{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		CreatedAt:   u.CreatedAt,
		Deleted:     u.DeletedAt != nil,
	}
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type tokenResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at"`
	User        User      `json:"user"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	req.Email = normalizeEmail(req.Email)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	fields := map[string]string{}
	if !validEmail(req.Email) {
		fields["email"] = "must be a valid email address"
	}
	if req.DisplayName == "" || len(req.DisplayName) > 128 {
		fields["display_name"] = "must be between 1 and 128 characters"
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		fields["password"] = err.Error()
	}
	if len(fields) > 0 {
		httpx.WriteProblem(w, r, httpx.Invalid(fields))
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	u, err := h.Store.CreateUser(r.Context(), sqlcgen.CreateUserParams{
		Email:        req.Email,
		PasswordHash: hash,
		DisplayName:  req.DisplayName,
	})
	if err != nil {
		// Relying on the constraint rather than a prior existence check: two
		// concurrent registrations would both pass such a check and one would
		// still land here.
		if store.IsUniqueViolation(err, "users_email_live_uniq") {
			httpx.WriteProblem(w, r, httpx.Errorf(
				http.StatusConflict, "an account with that email already exists"))
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}

	h.issue(w, r, u)
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	u, err := h.Store.GetUserByEmail(r.Context(), normalizeEmail(req.Email))
	if err != nil {
		if store.IsNotFound(err) {
			// Burn comparable time before failing. Returning immediately here
			// makes an unknown address measurably faster than a known one with
			// a wrong password, which is an account enumeration oracle that
			// identical error messages do not close.
			auth.DummyCompare(req.Password)
			httpx.WriteProblem(w, r, badCredentials())
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}

	if err := auth.CheckPassword(u.PasswordHash, req.Password); err != nil {
		httpx.WriteProblem(w, r, badCredentials())
		return
	}

	h.issue(w, r, u)
}

// badCredentials is one message for both "no such account" and "wrong
// password". Saying which would confirm whether an address is registered.
//
// Known gap: nothing throttles repeated failures here, so this endpoint is
// open to password brute force. That is not really rate limiting -- an edge
// WAF cannot tell a failed login from a successful one -- and the fix is
// failed-attempt throttling keyed on account plus source. See DISCUSSION 5.
func badCredentials() error {
	return httpx.Errorf(http.StatusUnauthorized, "invalid email or password")
}

func (h *Handler) issue(w http.ResponseWriter, r *http.Request, u sqlcgen.User) {
	token, expires, err := h.Tokens.Issue(u.ID, h.Now())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresAt:   expires,
		User:        toUser(u),
	})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, err := h.Store.GetUser(r.Context(), httpx.MustUserID(r.Context()))
	if err != nil {
		if store.IsNotFound(err) {
			// The token is valid but the account is gone -- deleted after the
			// token was issued.
			httpx.WriteProblem(w, r, httpx.Errorf(http.StatusUnauthorized, "account no longer exists"))
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUser(u))
}

type updateMeRequest struct {
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) {
	var req updateMeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	fields := map[string]string{}
	if req.Email != nil {
		*req.Email = normalizeEmail(*req.Email)
		if !validEmail(*req.Email) {
			fields["email"] = "must be a valid email address"
		}
	}
	if req.DisplayName != nil {
		*req.DisplayName = strings.TrimSpace(*req.DisplayName)
		if *req.DisplayName == "" || len(*req.DisplayName) > 128 {
			fields["display_name"] = "must be between 1 and 128 characters"
		}
	}
	if len(fields) > 0 {
		httpx.WriteProblem(w, r, httpx.Invalid(fields))
		return
	}

	u, err := h.Store.UpdateUserProfile(r.Context(), sqlcgen.UpdateUserProfileParams{
		ID:          httpx.MustUserID(r.Context()),
		DisplayName: req.DisplayName,
		Email:       req.Email,
	})
	if err != nil {
		switch {
		case store.IsUniqueViolation(err, "users_email_live_uniq"):
			httpx.WriteProblem(w, r, httpx.Errorf(
				http.StatusConflict, "an account with that email already exists"))
		case store.IsNotFound(err):
			httpx.WriteProblem(w, r, httpx.Errorf(http.StatusUnauthorized, "account no longer exists"))
		default:
			httpx.WriteProblem(w, r, err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUser(u))
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	userID := httpx.MustUserID(r.Context())
	u, err := h.Store.GetUser(r.Context(), userID)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusUnauthorized, "account no longer exists"))
		return
	}

	// Requiring the current password means a stolen token cannot be escalated
	// into permanent account takeover by changing the password outright.
	if err := auth.CheckPassword(u.PasswordHash, req.CurrentPassword); err != nil {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusForbidden, "current password is incorrect"))
		return
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{"new_password": err.Error()}))
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if _, err := h.Store.UpdateUserPassword(r.Context(), sqlcgen.UpdateUserPasswordParams{
		ID: userID, PasswordHash: hash,
	}); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Tokens already issued stay valid until they expire. Revoking them would
	// need a token store, which this build does not have. Named, not hidden.
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// deleteMe soft-deletes the account.
//
// Notes are NOT deleted. In a shared service they are not only yours -- hard
// deleting them would destroy other people's working context. The owner
// renders as a tombstone instead.
//
// This is self-service deletion only. Offboarding someone who left the
// company wants ownership transfer, and GDPR erasure wants anonymise-and-
// retain; both are out of scope, and offboarding is out of scope for a
// structural reason: the model has team admins but no org admin.
func (h *Handler) deleteMe(w http.ResponseWriter, r *http.Request) {
	userID := httpx.MustUserID(r.Context())

	err := h.Store.Tx(r.Context(), func(q *sqlcgen.Queries) error {
		n, err := q.SoftDeleteUser(r.Context(), userID)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("already deleted")
		}
		// Must be in the same transaction. note_shares.principal_id has no
		// foreign key -- it points at users or teams depending on
		// principal_type -- so nothing cascades. A grant left behind would
		// resolve against a reused UUID.
		_, err = q.DeleteUserShareGrants(r.Context(), userID)
		return err
	})
	if err != nil {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusUnauthorized, "account no longer exists"))
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// lookup finds a share target by exact email.
//
// Exact match only, and the query restricts results to people the caller
// already shares a team with. Prefix or fuzzy search over a user directory is
// a harvesting surface; this closes enumeration at design time for the cost of
// requiring the full address.
func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) {
	email := normalizeEmail(r.URL.Query().Get("email"))
	if email == "" {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusBadRequest,
			"the email query parameter is required; user search is exact-match only"))
		return
	}

	u, err := h.Store.FindUserByExactEmail(r.Context(), sqlcgen.FindUserByExactEmailParams{
		Email:    email,
		CallerID: httpx.MustUserID(r.Context()),
	})
	if err != nil {
		if store.IsNotFound(err) {
			// 404 whether the address is unregistered or simply belongs to
			// someone outside the caller's teams. Distinguishing the two would
			// turn this into the directory it is designed not to be.
			httpx.WriteProblem(w, r, httpx.NotFound("user"))
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUser(u))
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validEmail is deliberately loose. Full RFC 5322 validation rejects addresses
// that actually work; the real check is sending mail to it, which this service
// does not do.
func validEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.ContainsAny(s, " \t\r\n")
}
