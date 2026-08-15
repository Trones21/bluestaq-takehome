package notes

import (
	"net/http"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/authz"
	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Share is the wire representation of one grant.
type Share struct {
	PrincipalType string    `json:"principal_type"`
	PrincipalID   uuid.UUID `json:"principal_id"`
	PrincipalName string    `json:"principal_name"`
	Email         *string   `json:"email,omitempty"`
	Role          string    `json:"role"`
	CreatedAt     time.Time `json:"created_at"`
}

type shareRequest struct {
	PrincipalType string    `json:"principal_type"`
	PrincipalID   uuid.UUID `json:"principal_id"`
	Role          string    `json:"role"`
}

// listShares shows who can see this note.
//
// Viewer-level, not owner-only: knowing who else can read your note matters
// more than hiding the grant list from people who already have access.
func (h *Handler) listShares(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionListShares)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, err := h.Store.ListNoteShares(r.Context(), row.ID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]Share, 0, len(rows))
	for _, s := range rows {
		out = append(out, Share{
			PrincipalType: s.PrincipalType,
			PrincipalID:   s.PrincipalID,
			PrincipalName: s.PrincipalName,
			Email:         s.PrincipalEmail,
			Role:          s.Role,
			CreatedAt:     s.CreatedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"shares": out})
}

// upsertShare grants or changes access. Owner-only.
//
// Editors deliberately cannot re-share. Re-sharing rights are the usual source
// of "how did this leak" incidents, and owner-only keeps the access graph one
// hop deep and auditable.
func (h *Handler) upsertShare(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionShare)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var req shareRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Owner is not grantable -- it comes from notes.owner_user_id. Accepting it
	// here would let a grant manufacture a second owner.
	if _, err := authz.ParseGrantRole(req.Role); err != nil {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{
			"role": "must be viewer or editor",
		}))
		return
	}
	if req.PrincipalType != "user" && req.PrincipalType != "team" {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{
			"principal_type": "must be user or team",
		}))
		return
	}
	if req.PrincipalID == uuid.Nil {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{
			"principal_id": "is required",
		}))
		return
	}

	owner := httpx.MustUserID(r.Context())

	// Granting to yourself is a no-op that would look like it worked.
	if req.PrincipalType == "user" && req.PrincipalID == owner {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusBadRequest,
			"you already own this note"))
		return
	}

	// principal_id has no foreign key, so nothing stops a grant to a UUID that
	// names nothing. Validating here turns a typo into a 404 instead of a
	// permanently dead grant row.
	if err := h.validatePrincipal(r, req, owner); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if _, err := h.Store.UpsertNoteShare(r.Context(), sqlcgen.UpsertNoteShareParams{
		NoteID:        row.ID,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
		Role:          req.Role,
		GrantedBy:     owner,
	}); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

func (h *Handler) validatePrincipal(r *http.Request, req shareRequest, owner uuid.UUID) error {
	switch req.PrincipalType {
	case "user":
		exists, err := h.Store.UserExists(r.Context(), req.PrincipalID)
		if err != nil {
			return err
		}
		if !exists {
			return httpx.NotFound("user")
		}
	case "team":
		exists, err := h.Store.TeamExists(r.Context(), req.PrincipalID)
		if err != nil {
			return err
		}
		if !exists {
			return httpx.NotFound("team")
		}
		// You may only share with a team you belong to. Otherwise anyone could
		// push a note into an arbitrary team's view, and team IDs would be
		// probeable through the difference between 404 and 204.
		member, err := h.Store.IsTeamMember(r.Context(), sqlcgen.IsTeamMemberParams{
			TeamID: req.PrincipalID,
			UserID: owner,
		})
		if err != nil {
			return err
		}
		if !member {
			return httpx.NotFound("team")
		}
	}
	return nil
}

// revokeShare removes a grant. Owner-only, matching upsert.
func (h *Handler) revokeShare(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionRevokeShare)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	principalType := chi.URLParam(r, "principalType")
	if principalType != "user" && principalType != "team" {
		httpx.WriteProblem(w, r, httpx.NotFound("share"))
		return
	}
	principalID, err := uuid.Parse(chi.URLParam(r, "principalID"))
	if err != nil {
		httpx.WriteProblem(w, r, httpx.NotFound("share"))
		return
	}

	n, err := h.Store.DeleteNoteShare(r.Context(), sqlcgen.DeleteNoteShareParams{
		NoteID:        row.ID,
		PrincipalType: principalType,
		PrincipalID:   principalID,
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if n == 0 {
		httpx.WriteProblem(w, r, httpx.NotFound("share"))
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}
