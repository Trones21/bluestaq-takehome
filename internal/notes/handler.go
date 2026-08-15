// Package notes implements the note endpoints.
package notes

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/authz"
	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/Trones21/bluestaq-takehome/internal/obs"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	maxTitleLen = 512
	maxTags     = 32
	maxTagLen   = 64
)

type Handler struct {
	Store   *store.Store
	Metrics *obs.Metrics
}

func New(s *store.Store, m *obs.Metrics) *Handler {
	return &Handler{Store: s, Metrics: m}
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/notes", h.list)
	r.Post("/notes", h.create)
	r.Get("/notes/{id}", h.get)
	r.Patch("/notes/{id}", h.update)
	r.Delete("/notes/{id}", h.delete)
	r.Post("/notes/{id}/restore", h.restore)
	r.Get("/notes/{id}/shares", h.listShares)
	r.Put("/notes/{id}/shares", h.upsertShare)
	r.Delete("/notes/{id}/shares/{principalType}/{principalID}", h.revokeShare)
}

// Note is the wire representation.
type Note struct {
	ID          uuid.UUID  `json:"id"`
	OwnerUserID uuid.UUID  `json:"owner_user_id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Tags        []string   `json:"tags"`
	Version     int32      `json:"version"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`

	// YourRole tells the client what it may do without re-deriving the rules.
	// Without it every client reimplements the decision table, and they drift.
	YourRole string `json:"your_role"`
}

type createRequest struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	req.Title = strings.TrimSpace(req.Title)
	tags, err := normalizeTags(req.Tags)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{"tags": err.Error()}))
		return
	}
	if req.Title == "" || len(req.Title) > maxTitleLen {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{
			"title": fmt.Sprintf("must be between 1 and %d characters", maxTitleLen),
		}))
		return
	}

	n, err := h.Store.CreateNote(r.Context(), sqlcgen.CreateNoteParams{
		OwnerUserID: httpx.MustUserID(r.Context()),
		Title:       req.Title,
		Body:        req.Body,
		Tags:        tags,
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := fromCreate(n)
	setETag(w, out.Version)
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	row, role, err := h.load(r, authz.ActionRead)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := fromAccess(row, role)
	setETag(w, out.Version)
	httpx.WriteJSON(w, http.StatusOK, out)
}

type updateRequest struct {
	Title *string   `json:"title"`
	Body  *string   `json:"body"`
	Tags  *[]string `json:"tags"`
}

// update applies a partial change under optimistic concurrency.
//
// If-Match is required, not optional. Making it optional would mean the
// careless client -- the one most likely to clobber someone -- is exactly the
// one that skips the check.
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	row, role, err := h.load(r, authz.ActionUpdate)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	expected, err := ifMatchVersion(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	params := sqlcgen.UpdateNoteParams{ID: row.ID, ExpectedVersion: expected}
	fields := map[string]string{}

	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if t == "" || len(t) > maxTitleLen {
			fields["title"] = fmt.Sprintf("must be between 1 and %d characters", maxTitleLen)
		}
		params.Title = &t
	}
	if req.Body != nil {
		params.Body = req.Body
	}
	if req.Tags != nil {
		tags, err := normalizeTags(*req.Tags)
		if err != nil {
			fields["tags"] = err.Error()
		}
		params.Tags = tags
	}
	if len(fields) > 0 {
		httpx.WriteProblem(w, r, httpx.Invalid(fields))
		return
	}

	updated, err := h.Store.UpdateNote(r.Context(), params)
	if err != nil {
		if store.IsNotFound(err) {
			// Zero rows: the version predicate did not match. Either someone
			// committed first or the note was deleted underneath us. Re-read to
			// find out which, and hand the client the current state so it can
			// show a diff rather than just an error.
			httpx.WriteProblem(w, r, h.conflict(r.Context(), row.ID, httpx.MustUserID(r.Context()), expected))
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}

	out := fromUpdate(updated, role)
	setETag(w, out.Version)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// conflict distinguishes a lost race from a deleted note, and builds the 409
// body. A bare "conflict" forces the client to guess.
func (h *Handler) conflict(ctx context.Context, noteID, userID uuid.UUID, expected int32) error {
	row, err := h.Store.GetNoteWithAccess(ctx, sqlcgen.GetNoteWithAccessParams{
		UserID: userID, NoteID: noteID,
	})
	if err != nil || row.DeletedAt != nil {
		return httpx.NotFound("note")
	}

	h.Metrics.Conflicts.Inc()

	p := httpx.Errorf(http.StatusConflict,
		"the note has changed since you read it (you sent version %d, current is %d)",
		expected, row.Version)
	p.Conflict = fromAccess(row, authz.Role(row.EffectiveRole))
	return p
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionDelete)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	n, err := h.Store.SoftDeleteNote(r.Context(), row.ID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if n == 0 {
		// Already deleted. Idempotent from the caller's point of view.
		httpx.WriteJSON(w, http.StatusNoContent, nil)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// restore undoes a soft delete. Owner-only, matching delete -- an editor
// undoing an owner's deletion would be surprising.
func (h *Handler) restore(w http.ResponseWriter, r *http.Request) {
	row, role, err := h.load(r, authz.ActionRestore)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if row.DeletedAt == nil {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusConflict, "the note is not deleted"))
		return
	}

	restored, err := h.Store.RestoreNote(r.Context(), row.ID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := fromRestore(restored, role)
	setETag(w, out.Version)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// load resolves the note and the caller's role in one query, then enforces the
// decision table.
//
// Two rules live here, and every handler inherits them:
//
//   - No read access returns 404, never 403. A 403 confirms the note exists,
//     which turns ID enumeration into a way to learn about other teams' data.
//   - Insufficient role for a specific action returns 403, because the caller
//     can already see the note -- its existence is not a secret from them.
//
// A soft-deleted note reads as absent for every action except restore, so a
// deleted note disappears for everyone it was shared with.
func (h *Handler) load(r *http.Request, action authz.Action) (sqlcgen.GetNoteWithAccessRow, authz.Role, error) {
	var zero sqlcgen.GetNoteWithAccessRow

	noteID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		// A malformed ID is reported as absent rather than as a bad request:
		// "that is not a valid note id" and "no such note" should look alike.
		return zero, authz.RoleNone, httpx.NotFound("note")
	}

	row, err := h.Store.GetNoteWithAccess(r.Context(), sqlcgen.GetNoteWithAccessParams{
		UserID: httpx.MustUserID(r.Context()),
		NoteID: noteID,
	})
	if err != nil {
		if store.IsNotFound(err) {
			return zero, authz.RoleNone, httpx.NotFound("note")
		}
		return zero, authz.RoleNone, err
	}

	role := authz.Role(row.EffectiveRole)
	if !authz.Can(role, authz.ActionRead) {
		return zero, authz.RoleNone, httpx.NotFound("note")
	}
	if row.DeletedAt != nil && action != authz.ActionRestore {
		return zero, authz.RoleNone, httpx.NotFound("note")
	}
	if !authz.Can(role, action) {
		return zero, role, httpx.Forbidden(action.String())
	}
	return row, role, nil
}

// setETag publishes the version as an entity tag, which is where If-Match
// reads it back from.
func setETag(w http.ResponseWriter, version int32) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(int(version))))
}

// ifMatchVersion parses the required If-Match header.
//
// A missing header is 428 Precondition Required rather than 400: the request
// is well-formed, it just refuses to proceed unconditionally, and 428 exists
// to say exactly that.
func ifMatchVersion(r *http.Request) (int32, error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, httpx.Errorf(http.StatusPreconditionRequired,
			"an If-Match header carrying the note's current version is required; "+
				"read the note first and send back its ETag")
	}
	// A wildcard means "any existing version", which is precisely the
	// unconditional overwrite this endpoint exists to prevent.
	if raw == "*" {
		return 0, httpx.Errorf(http.StatusPreconditionRequired,
			`If-Match: * is not accepted; send the note's current version`)
	}

	// Accept both the quoted entity-tag form and a bare number, since clients
	// hand-writing requests routinely send the latter.
	unquoted := strings.Trim(strings.TrimPrefix(raw, "W/"), `"`)
	v, err := strconv.ParseInt(unquoted, 10, 32)
	if err != nil || v < 1 {
		return 0, httpx.Errorf(http.StatusBadRequest,
			"If-Match must be the note's version number, got %q", raw)
	}
	return int32(v), nil
}

// normalizeTags trims, lowercases, drops blanks and de-duplicates.
//
// Lowercasing means "Postgres" and "postgres" are one tag rather than two
// that look identical in a list. Never returns nil: a nil slice would make
// coalesce() in the update statement treat "clear all tags" as "leave
// unchanged", so clearing tags would silently do nothing.
func normalizeTags(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))

	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if len(t) > maxTagLen {
			return nil, fmt.Errorf("tag %q exceeds %d characters", t, maxTagLen)
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, fmt.Errorf("a note may carry at most %d tags", maxTags)
	}
	return out, nil
}
