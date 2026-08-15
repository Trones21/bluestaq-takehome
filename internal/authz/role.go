// Package authz holds the note access rules.
//
// One rule, in one place, exercised by every endpoint. This is the code where
// a bug is a data leak rather than a broken feature, so the decision table is
// explicit and exhaustively tested rather than spread across handlers.
package authz

import "fmt"

// Role is a caller's effective access to a single note.
//
// Ranked as integers deliberately. The database resolves the highest matching
// grant with a numeric max, because ordering role *names* in SQL sorts
// alphabetically -- under which "viewer" outranks "editor", silently
// escalating every viewer on a note that has both grants.
type Role int

const (
	RoleNone   Role = 0
	RoleViewer Role = 1
	RoleEditor Role = 2
	RoleOwner  Role = 3
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleEditor:
		return "editor"
	case RoleOwner:
		return "owner"
	default:
		return "none"
	}
}

// ParseGrantRole converts a stored note_shares.role value. Owner is not a
// grantable role -- it comes from notes.owner_user_id -- so it is rejected
// here rather than silently accepted.
func ParseGrantRole(s string) (Role, error) {
	switch s {
	case "viewer":
		return RoleViewer, nil
	case "editor":
		return RoleEditor, nil
	default:
		return RoleNone, fmt.Errorf("invalid grant role %q: must be viewer or editor", s)
	}
}

// Action is something a caller may attempt against a note.
type Action int

const (
	ActionRead Action = iota
	ActionListShares
	ActionDownloadAttachment
	ActionUpdate
	ActionAddAttachment
	ActionRemoveAttachment
	ActionDelete
	ActionRestore
	ActionShare
	ActionRevokeShare
)

func (a Action) String() string {
	switch a {
	case ActionRead:
		return "read this note"
	case ActionListShares:
		return "view this note's sharing"
	case ActionDownloadAttachment:
		return "download this attachment"
	case ActionUpdate:
		return "update this note"
	case ActionAddAttachment:
		return "add an attachment"
	case ActionRemoveAttachment:
		return "remove an attachment"
	case ActionDelete:
		return "delete this note"
	case ActionRestore:
		return "restore this note"
	case ActionShare:
		return "share this note"
	case ActionRevokeShare:
		return "revoke access to this note"
	default:
		return "perform this action"
	}
}

// minimumRole is the decision table. Every access rule in the service lives
// here; handlers ask, they do not decide.
//
// Three choices worth defending:
//
//   - Editors cannot share. Re-sharing rights are the usual source of "how did
//     this leak" incidents; owner-only keeps the access graph one hop deep and
//     auditable.
//   - Viewers can see who else a note is shared with. Knowing who can read
//     your note matters more than concealing the grant list from people who
//     already have access.
//   - Restore is owner-only, matching delete. An editor undoing an owner's
//     delete is surprising, and the pair should not be asymmetric.
//
// Team role is absent on purpose: team_members.role governs membership
// management only. A team admin is not automatically an editor of every note
// shared with the team -- conflating the two is a common source of surprise.
var minimumRole = map[Action]Role{
	ActionRead:               RoleViewer,
	ActionListShares:         RoleViewer,
	ActionDownloadAttachment: RoleViewer,

	ActionUpdate:           RoleEditor,
	ActionAddAttachment:    RoleEditor,
	ActionRemoveAttachment: RoleEditor,

	ActionDelete:      RoleOwner,
	ActionRestore:     RoleOwner,
	ActionShare:       RoleOwner,
	ActionRevokeShare: RoleOwner,
}

// Can reports whether role permits action.
//
// An action missing from the table denies rather than allows. That way adding
// an Action constant without a rule fails closed -- a new endpoint that nobody
// wired up returns 403 instead of granting the world access.
func Can(role Role, action Action) bool {
	required, known := minimumRole[action]
	if !known {
		return false
	}
	return role >= required
}

// Actions returns every action, for exhaustive testing.
func Actions() []Action {
	return []Action{
		ActionRead, ActionListShares, ActionDownloadAttachment,
		ActionUpdate, ActionAddAttachment, ActionRemoveAttachment,
		ActionDelete, ActionRestore, ActionShare, ActionRevokeShare,
	}
}

// Roles returns every role, for exhaustive testing.
func Roles() []Role {
	return []Role{RoleNone, RoleViewer, RoleEditor, RoleOwner}
}
