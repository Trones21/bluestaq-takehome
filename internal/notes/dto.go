package notes

import (
	"github.com/Trones21/bluestaq-takehome/internal/authz"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
)

// sqlc emits a distinct row type per query even when the column list is
// identical, so each one needs its own conversion. Tedious, and the tedium is
// the point: the wire shape is written by hand, so a column added to `notes`
// cannot appear in a response by accident.

func fromCreate(n sqlcgen.CreateNoteRow) Note {
	// The creator is the owner by definition; no need to resolve it.
	return Note{
		ID: n.ID, OwnerUserID: n.OwnerUserID,
		Title: n.Title, Body: n.Body, Tags: n.Tags, Version: n.Version,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, DeletedAt: n.DeletedAt,
		YourRole: authz.RoleOwner.String(),
	}
}

func fromAccess(n sqlcgen.GetNoteWithAccessRow, role authz.Role) Note {
	return Note{
		ID: n.ID, OwnerUserID: n.OwnerUserID,
		Title: n.Title, Body: n.Body, Tags: n.Tags, Version: n.Version,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, DeletedAt: n.DeletedAt,
		YourRole: role.String(),
	}
}

func fromUpdate(n sqlcgen.UpdateNoteRow, role authz.Role) Note {
	return Note{
		ID: n.ID, OwnerUserID: n.OwnerUserID,
		Title: n.Title, Body: n.Body, Tags: n.Tags, Version: n.Version,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, DeletedAt: n.DeletedAt,
		YourRole: role.String(),
	}
}

func fromRestore(n sqlcgen.RestoreNoteRow, role authz.Role) Note {
	return Note{
		ID: n.ID, OwnerUserID: n.OwnerUserID,
		Title: n.Title, Body: n.Body, Tags: n.Tags, Version: n.Version,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, DeletedAt: n.DeletedAt,
		YourRole: role.String(),
	}
}
