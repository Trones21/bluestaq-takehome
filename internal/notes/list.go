package notes

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Trones21/bluestaq-takehome/internal/authz"
	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/google/uuid"
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

// ListItem is a note as it appears in a list.
//
// Attachment URLs are deliberately absent, and the count stands in for them.
// Presigning is a per-object operation, so embedding download URLs would mean
// one presign call per attachment per page -- 50 signatures to render a screen
// that displays none of them. URLs come from GET /notes/{id} and the
// attachments endpoint, where they are actually about to be used.
type ListItem struct {
	Note
	AttachmentCount int64 `json:"attachment_count"`
}

type listResponse struct {
	Notes      []ListItem `json:"notes"`
	NextCursor string     `json:"next_cursor,omitempty"`
	HasMore    bool       `json:"has_more"`
}

// list returns one page of notes the caller may read.
//
// The invariant every filter obeys: it narrows the already-authorized set.
// ?owner=bob returns Bob's notes I can already see, never all of Bob's. A new
// filter is a new AND, never a new way in.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	filter, err := parseListFilter(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, next, err := h.Store.ListNotes(r.Context(), filter)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]ListItem, 0, len(rows))
	for _, row := range rows {
		role := authz.Role(row.EffectiveRole)
		out = append(out, ListItem{
			Note: Note{
				ID: row.ID, OwnerUserID: row.OwnerUserID,
				Title: row.Title, Body: row.Body, Tags: row.Tags,
				Version:   row.Version,
				CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
				DeletedAt: row.DeletedAt,
				YourRole:  role.String(),
			},
			AttachmentCount: row.Attachments,
		})
	}

	res := listResponse{Notes: out}
	if next != nil {
		res.NextCursor = next.Encode()
		res.HasMore = true
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func parseListFilter(r *http.Request) (store.ListFilter, error) {
	q := r.URL.Query()
	f := store.ListFilter{
		UserID:       httpx.MustUserID(r.Context()),
		Query:        strings.TrimSpace(q.Get("q")),
		SharedWithMe: q.Get("shared_with_me") == "true",
		Deleted:      q.Get("deleted") == "true",
		Limit:        defaultLimit,
	}

	// Repeatable, with AND semantics: ?tag=db&tag=perf means both.
	for _, t := range q["tag"] {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			f.Tags = append(f.Tags, t)
		}
	}

	// "me" is sugar for the caller's own id, so the common case does not
	// require the client to substitute its own UUID into a URL.
	if raw := q.Get("owner"); raw != "" {
		if raw == "me" {
			id := f.UserID
			f.Owner = &id
		} else {
			id, err := uuid.Parse(raw)
			if err != nil {
				return f, httpx.Invalid(map[string]string{"owner": "must be a uuid or \"me\""})
			}
			f.Owner = &id
		}
	}

	if raw := q.Get("team"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return f, httpx.Invalid(map[string]string{"team": "must be a uuid"})
		}
		f.Team = &id
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return f, httpx.Invalid(map[string]string{"limit": "must be a positive integer"})
		}
		// Clamped rather than rejected: a client asking for 10,000 gets the
		// maximum page instead of an error, and the server keeps control of
		// how much work one request can cost.
		if n > maxLimit {
			n = maxLimit
		}
		f.Limit = int32(n)
	}

	if raw := q.Get("cursor"); raw != "" {
		c, err := store.DecodeCursor(raw)
		if err != nil {
			return f, httpx.Invalid(map[string]string{
				"cursor": "malformed; use the next_cursor from a previous response",
			})
		}
		f.Cursor = c
	}

	// Mutually exclusive rather than silently empty: owner=someone-else plus
	// shared_with_me can only ever return nothing, and returning an empty page
	// for a contradictory query looks like missing data.
	if f.SharedWithMe && f.Owner != nil && *f.Owner == f.UserID {
		return f, httpx.Invalid(map[string]string{
			"shared_with_me": "cannot combine with owner=me; they exclude each other",
		})
	}

	return f, nil
}
