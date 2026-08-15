// Package teams implements team and membership endpoints.
package teams

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxNameLen = 128

// Membership roles. These govern who may manage the team -- they are
// deliberately NOT note permissions. A team admin is not automatically an
// editor of every note shared with the team; access to a note comes from the
// grant, never from the membership role.
const (
	roleMember = "member"
	roleAdmin  = "admin"
)

type Handler struct {
	Store *store.Store
}

func New(s *store.Store) *Handler { return &Handler{Store: s} }

func (h *Handler) Routes(r chi.Router) {
	r.Get("/teams", h.list)
	r.Post("/teams", h.create)
	r.Get("/teams/{id}", h.get)
	r.Patch("/teams/{id}", h.update)
	r.Delete("/teams/{id}", h.delete)
	r.Get("/teams/{id}/members", h.listMembers)
	r.Post("/teams/{id}/members", h.addMember)
	r.Delete("/teams/{id}/members/{userID}", h.removeMember)
	r.Post("/teams/{id}/leave", h.leave)
}

type Team struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	YourRole  string    `json:"your_role,omitempty"`
}

type Member struct {
	UserID      uuid.UUID `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joined_at"`
}

type nameRequest struct {
	Name string `json:"name"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req nameRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	name, err := validName(req.Name)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	userID := httpx.MustUserID(r.Context())
	var team sqlcgen.Team

	// The creator becomes the first admin. Both statements or neither -- a
	// team with no members is unreachable and unmanageable.
	err = h.Store.Tx(r.Context(), func(q *sqlcgen.Queries) error {
		var err error
		team, err = q.CreateTeam(r.Context(), name)
		if err != nil {
			return err
		}
		_, err = q.AddTeamMember(r.Context(), sqlcgen.AddTeamMemberParams{
			TeamID: team.ID, UserID: userID, Role: roleAdmin,
		})
		return err
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, Team{
		ID: team.ID, Name: team.Name,
		CreatedAt: team.CreatedAt, UpdatedAt: team.UpdatedAt,
		YourRole: roleAdmin,
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.ListMyTeams(r.Context(), httpx.MustUserID(r.Context()))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]Team, 0, len(rows))
	for _, t := range rows {
		out = append(out, Team{
			ID: t.ID, Name: t.Name,
			CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, YourRole: t.Role,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"teams": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	teamID, role, err := h.membership(r, roleMember)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	t, err := h.Store.GetTeam(r.Context(), teamID)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.NotFound("team"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, Team{
		ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, YourRole: role,
	})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	teamID, _, err := h.membership(r, roleAdmin)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var req nameRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	name, err := validName(req.Name)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	t, err := h.Store.UpdateTeam(r.Context(), sqlcgen.UpdateTeamParams{ID: teamID, Name: name})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, Team{
		ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, YourRole: roleAdmin,
	})
}

// delete removes the team and every grant naming it.
//
// Notes are untouched: they belong to their owners, and a team is only ever a
// grantee. What must not survive is the grant itself -- principal_id has no
// foreign key, so nothing cascades, and an orphaned ('team', id) row would
// grant access to whoever next receives that UUID.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	teamID, _, err := h.membership(r, roleAdmin)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	err = h.Store.Tx(r.Context(), func(q *sqlcgen.Queries) error {
		if _, err := q.DeleteTeamShareGrants(r.Context(), teamID); err != nil {
			return err
		}
		n, err := q.DeleteTeam(r.Context(), teamID)
		if err != nil {
			return err
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
	if err != nil {
		if store.IsNotFound(err) {
			httpx.WriteProblem(w, r, httpx.NotFound("team"))
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	teamID, _, err := h.membership(r, roleMember)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, err := h.Store.ListTeamMembers(r.Context(), teamID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]Member, 0, len(rows))
	for _, m := range rows {
		out = append(out, Member{
			UserID: m.ID, Email: m.Email, DisplayName: m.DisplayName,
			Role: m.Role, JoinedAt: m.CreatedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"members": out})
}

type addMemberRequest struct {
	UserID uuid.UUID `json:"user_id"`
	Role   string    `json:"role"`
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	teamID, _, err := h.membership(r, roleAdmin)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var req addMemberRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if req.Role == "" {
		req.Role = roleMember
	}
	if req.Role != roleMember && req.Role != roleAdmin {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{
			"role": "must be member or admin",
		}))
		return
	}
	if req.UserID == uuid.Nil {
		httpx.WriteProblem(w, r, httpx.Invalid(map[string]string{
			"user_id": "is required; find the user with GET /v1/users?email=",
		}))
		return
	}

	// Adding an existing member updates their role, so this doubles as the
	// promote/demote endpoint. Demoting the last admin must still be refused.
	err = h.Store.Tx(r.Context(), func(q *sqlcgen.Queries) error {
		if req.Role == roleMember {
			if err := guardLastAdmin(r.Context(), q, teamID, req.UserID); err != nil {
				return err
			}
		}
		_, err := q.AddTeamMember(r.Context(), sqlcgen.AddTeamMemberParams{
			TeamID: teamID, UserID: req.UserID, Role: req.Role,
		})
		return err
	})
	if err != nil {
		if store.IsForeignKeyViolation(err) {
			httpx.WriteProblem(w, r, httpx.NotFound("user"))
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	teamID, _, err := h.membership(r, roleAdmin)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	target, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		httpx.WriteProblem(w, r, httpx.NotFound("member"))
		return
	}

	if err := h.removeFromTeam(r.Context(), teamID, target); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// leave is self-service removal, subject to the same last-admin guard.
func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	teamID, _, err := h.membership(r, roleMember)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if err := h.removeFromTeam(r.Context(), teamID, httpx.MustUserID(r.Context())); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// removeFromTeam drops a membership, refusing to strand the team.
//
// Note what this does NOT do: it leaves note_shares alone. The grant is to the
// team, not to the person, so it stays valid for whoever remains. The
// departing member simply stops matching it -- which is why removal revokes
// their access immediately, with nothing to clean up.
func (h *Handler) removeFromTeam(ctx context.Context, teamID, userID uuid.UUID) error {
	return h.Store.Tx(ctx, func(q *sqlcgen.Queries) error {
		if err := guardLastAdmin(ctx, q, teamID, userID); err != nil {
			return err
		}
		n, err := q.RemoveTeamMember(ctx, sqlcgen.RemoveTeamMemberParams{
			TeamID: teamID, UserID: userID,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return httpx.NotFound("member")
		}
		return nil
	})
}

// guardLastAdmin refuses an operation that would leave a team with no admins.
//
// The count runs inside the caller's transaction. Outside one, two concurrent
// removals could each see two admins, each conclude it is safe, and together
// strand the team with zero -- after which nobody can add a member, rename it,
// or delete it.
func guardLastAdmin(ctx context.Context, q *sqlcgen.Queries, teamID, userID uuid.UUID) error {
	role, err := q.GetMembership(ctx, sqlcgen.GetMembershipParams{TeamID: teamID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not a member; nothing to strand
		}
		return err
	}
	if role != roleAdmin {
		return nil
	}

	admins, err := q.CountTeamAdmins(ctx, teamID)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return httpx.Errorf(http.StatusConflict,
			"this is the team's last admin; promote another member first")
	}
	return nil
}

// membership resolves the team from the URL and enforces the required role.
//
// A non-member gets 404 rather than 403, for the same reason notes do: a 403
// confirms the team exists, and team names leak organisational structure.
// An insufficient role gets 403, because a member already knows it exists.
func (h *Handler) membership(r *http.Request, required string) (uuid.UUID, string, error) {
	teamID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, "", httpx.NotFound("team")
	}

	role, err := h.Store.GetMembership(r.Context(), sqlcgen.GetMembershipParams{
		TeamID: teamID, UserID: httpx.MustUserID(r.Context()),
	})
	if err != nil {
		if store.IsNotFound(err) {
			return uuid.Nil, "", httpx.NotFound("team")
		}
		return uuid.Nil, "", err
	}
	if required == roleAdmin && role != roleAdmin {
		return teamID, role, httpx.Forbidden("manage this team")
	}
	return teamID, role, nil
}

func validName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxNameLen {
		return "", httpx.Invalid(map[string]string{
			"name": "must be between 1 and 128 characters",
		})
	}
	return s, nil
}
