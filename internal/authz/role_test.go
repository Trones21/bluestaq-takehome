package authz

import "testing"

// TestDecisionMatrix is the whole rule set, written out by hand rather than
// derived from the implementation. Deriving it would make the test agree with
// whatever the code does, including the bugs.
//
// This is the table a reviewer should read to know what the service permits.
func TestDecisionMatrix(t *testing.T) {
	// want[action][role]
	want := map[Action]map[Role]bool{
		ActionRead: {
			RoleNone: false, RoleViewer: true, RoleEditor: true, RoleOwner: true,
		},
		ActionListShares: {
			RoleNone: false, RoleViewer: true, RoleEditor: true, RoleOwner: true,
		},
		ActionDownloadAttachment: {
			RoleNone: false, RoleViewer: true, RoleEditor: true, RoleOwner: true,
		},
		ActionUpdate: {
			RoleNone: false, RoleViewer: false, RoleEditor: true, RoleOwner: true,
		},
		ActionAddAttachment: {
			RoleNone: false, RoleViewer: false, RoleEditor: true, RoleOwner: true,
		},
		ActionRemoveAttachment: {
			RoleNone: false, RoleViewer: false, RoleEditor: true, RoleOwner: true,
		},
		ActionDelete: {
			RoleNone: false, RoleViewer: false, RoleEditor: false, RoleOwner: true,
		},
		ActionRestore: {
			RoleNone: false, RoleViewer: false, RoleEditor: false, RoleOwner: true,
		},
		ActionShare: {
			RoleNone: false, RoleViewer: false, RoleEditor: false, RoleOwner: true,
		},
		ActionRevokeShare: {
			RoleNone: false, RoleViewer: false, RoleEditor: false, RoleOwner: true,
		},
	}

	// Every action must appear, so adding one without a rule fails here rather
	// than shipping with undefined behaviour.
	if len(want) != len(Actions()) {
		t.Fatalf("matrix covers %d actions but %d exist -- a new action has no expected behaviour",
			len(want), len(Actions()))
	}

	for _, action := range Actions() {
		expectations, ok := want[action]
		if !ok {
			t.Errorf("action %q missing from the matrix", action)
			continue
		}
		for _, role := range Roles() {
			expected, ok := expectations[role]
			if !ok {
				t.Errorf("action %q has no expectation for role %q", action, role)
				continue
			}
			if got := Can(role, action); got != expected {
				t.Errorf("Can(%s, %q) = %v, want %v", role, action, got, expected)
			}
		}
	}
}

// RoleNone is the answer for a stranger, and it must permit nothing at all.
// A single missing entry in the decision table would show up here first.
func TestNoRoleCanDoNothing(t *testing.T) {
	for _, action := range Actions() {
		if Can(RoleNone, action) {
			t.Errorf("a user with no grant may %q", action)
		}
	}
}

// An unmapped action must deny. Adding an Action constant and forgetting the
// rule should return 403, not grant the world access.
func TestUnknownActionFailsClosed(t *testing.T) {
	unmapped := Action(9999)
	for _, role := range Roles() {
		if Can(role, unmapped) {
			t.Errorf("unmapped action permitted for role %s -- the table fails open", role)
		}
	}
}

// Roles must be totally ordered, because the database resolves the highest
// matching grant numerically. If these ever stop ascending, "max grant wins"
// silently picks the wrong one.
func TestRolesAreOrdered(t *testing.T) {
	if !(RoleNone < RoleViewer && RoleViewer < RoleEditor && RoleEditor < RoleOwner) {
		t.Fatal("role ranks are not strictly ascending")
	}
}

// The reason ranks are integers rather than strings: sorted as text, "viewer"
// is greater than "editor", so a max() over role names would promote every
// viewer on a note carrying both grants.
func TestAlphabeticalOrderWouldEscalateViewers(t *testing.T) {
	if !("viewer" > "editor") {
		t.Skip("string ordering assumption no longer holds")
	}
	if RoleViewer > RoleEditor {
		t.Fatal("roles are ordered alphabetically -- viewers outrank editors")
	}
}

// Access is strictly monotonic: anything a lesser role may do, a greater role
// may also do. A table entry that broke this would mean, say, an editor able
// to read but an owner not.
func TestHigherRolesSubsumeLowerOnes(t *testing.T) {
	ordered := []Role{RoleNone, RoleViewer, RoleEditor, RoleOwner}
	for _, action := range Actions() {
		for i := 1; i < len(ordered); i++ {
			lower, higher := ordered[i-1], ordered[i]
			if Can(lower, action) && !Can(higher, action) {
				t.Errorf("%s may %q but %s may not", lower, action, higher)
			}
		}
	}
}

// Sharing is owner-only by design: re-sharing rights are the usual source of
// accidental exposure, and owner-only keeps the access graph one hop deep.
func TestEditorsCannotReshare(t *testing.T) {
	for _, action := range []Action{ActionShare, ActionRevokeShare} {
		if Can(RoleEditor, action) {
			t.Errorf("an editor may %q; re-sharing must be owner-only", action)
		}
	}
}

// Delete and restore must stay symmetric. If restore were merely editor-level,
// an editor could undo an owner's delete.
func TestDeleteAndRestoreRequireTheSameRole(t *testing.T) {
	for _, role := range Roles() {
		if Can(role, ActionDelete) != Can(role, ActionRestore) {
			t.Errorf("role %s: delete=%v restore=%v -- these must match",
				role, Can(role, ActionDelete), Can(role, ActionRestore))
		}
	}
}

func TestParseGrantRoleRejectsOwner(t *testing.T) {
	// Owner comes from notes.owner_user_id, never from a grant row. Accepting
	// it here would let a share grant manufacture a second owner.
	if _, err := ParseGrantRole("owner"); err == nil {
		t.Fatal("owner was accepted as a grantable role")
	}
	for _, bad := range []string{"", "admin", "Viewer", "editor "} {
		if _, err := ParseGrantRole(bad); err == nil {
			t.Errorf("ParseGrantRole(%q) accepted", bad)
		}
	}
	for in, want := range map[string]Role{"viewer": RoleViewer, "editor": RoleEditor} {
		got, err := ParseGrantRole(in)
		if err != nil || got != want {
			t.Errorf("ParseGrantRole(%q) = %v, %v", in, got, err)
		}
	}
}
