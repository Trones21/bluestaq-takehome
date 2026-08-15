package notes_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Trones21/bluestaq-takehome/internal/apitest"
	"github.com/google/uuid"
)

type note struct {
	ID       uuid.UUID `json:"id"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Tags     []string  `json:"tags"`
	Version  int32     `json:"version"`
	YourRole string    `json:"your_role"`
}

func createNote(t *testing.T, c *apitest.Client, title string) note {
	t.Helper()
	var n note
	c.Post("/v1/notes", map[string]any{"title": title, "body": "initial body"}).
		Require(http.StatusCreated).JSON(&n)
	return n
}

func TestCreateAndReadOwnNote(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	res := alice.Post("/v1/notes", map[string]any{
		"title": "Design doc", "body": "contents", "tags": []string{"Spec", "spec", " design "},
	}).Require(http.StatusCreated)

	var n note
	res.JSON(&n)

	if n.Version != 1 {
		t.Errorf("new note version = %d, want 1", n.Version)
	}
	if res.ETag() != "1" {
		t.Errorf("ETag = %q, want \"1\"", res.ETag())
	}
	if n.YourRole != "owner" {
		t.Errorf("your_role = %q, want owner", n.YourRole)
	}
	// Tags are lowercased, trimmed and de-duplicated, so "Spec" and "spec" do
	// not become two entries that look identical in a list.
	if len(n.Tags) != 2 || n.Tags[0] != "spec" || n.Tags[1] != "design" {
		t.Errorf("tags = %v, want [spec design]", n.Tags)
	}
}

// The core privacy guarantee: a note with no grants is invisible, and reported
// as absent rather than forbidden.
func TestStrangerCannotSeePrivateNoteAndGets404(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	n := createNote(t, alice, "Alice's private note")

	res := bob.Get("/v1/notes/" + n.ID.String())
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404 -- a 403 would confirm the note exists", res.StatusCode)
	}
	// The response must not echo anything about the note.
	if strings.Contains(string(res.Body), "Alice's private note") {
		t.Error("404 body leaked the note title")
	}
}

func TestUnknownAndMalformedIDsBothLookAbsent(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	for _, id := range []string{uuid.NewString(), "not-a-uuid", "12345"} {
		if got := alice.Get("/v1/notes/" + id).StatusCode; got != http.StatusNotFound {
			t.Errorf("GET /v1/notes/%s = %d, want 404", id, got)
		}
	}
}

// The headline design choice: two people edit the same note, and the second
// save is rejected rather than silently overwriting the first.
func TestConcurrentEditSecondWriterGets409(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	n := createNote(t, alice, "Shared note")
	shareWith(t, env, n.ID, alice, bob, "editor")

	// Both read version 1.
	aliceRead := alice.Get("/v1/notes/" + n.ID.String()).Require(http.StatusOK)
	bobRead := bob.Get("/v1/notes/" + n.ID.String()).Require(http.StatusOK)
	if aliceRead.ETag() != bobRead.ETag() {
		t.Fatalf("readers disagree on version: %q vs %q", aliceRead.ETag(), bobRead.ETag())
	}

	// Alice saves first and wins.
	alice.Patch("/v1/notes/"+n.ID.String(), map[string]any{"body": "alice's work"},
		"If-Match", aliceRead.ETag()).Require(http.StatusOK)

	// Bob saves against the version he read, and is refused.
	conflict := bob.Patch("/v1/notes/"+n.ID.String(), map[string]any{"body": "bob's work"},
		"If-Match", bobRead.ETag()).Require(http.StatusConflict)

	// The 409 must carry the current state, so the client can show a diff
	// rather than just an error.
	var body struct {
		Conflict note `json:"conflict"`
	}
	conflict.JSON(&body)
	if body.Conflict.Body != "alice's work" {
		t.Errorf("conflict body = %q, want the winning version's content", body.Conflict.Body)
	}
	if body.Conflict.Version != 2 {
		t.Errorf("conflict version = %d, want 2", body.Conflict.Version)
	}

	// And Alice's work must still be there -- the whole point.
	var current note
	alice.Get("/v1/notes/" + n.ID.String()).Require(http.StatusOK).JSON(&current)
	if current.Body != "alice's work" {
		t.Fatalf("body = %q, want alice's work to have survived", current.Body)
	}
}

// Under real concurrency exactly one writer may win. This is the property the
// single conditional UPDATE buys: check and write are one atomic statement.
func TestOnlyOneOfManyConcurrentWritersWins(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	n := createNote(t, alice, "Contended note")

	const writers = 8
	var wg sync.WaitGroup
	results := make([]int, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = alice.Patch("/v1/notes/"+n.ID.String(),
				map[string]any{"body": "writer"}, "If-Match", "1").StatusCode
		}(i)
	}
	wg.Wait()

	var ok, conflicts int
	for _, code := range results {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if ok != 1 {
		t.Fatalf("%d writers succeeded against version 1, want exactly 1", ok)
	}
	if conflicts != writers-1 {
		t.Fatalf("got %d conflicts, want %d", conflicts, writers-1)
	}
}

func TestUpdateRequiresIfMatch(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	n := createNote(t, alice, "note")

	// Missing precondition is 428, not 400: the request is well-formed, it
	// just refuses to proceed unconditionally.
	res := alice.Patch("/v1/notes/"+n.ID.String(), map[string]any{"body": "x"})
	if res.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("got %d, want 428", res.StatusCode)
	}

	// A wildcard is an unconditional overwrite, which is what this prevents.
	res = alice.Patch("/v1/notes/"+n.ID.String(), map[string]any{"body": "x"}, "If-Match", "*")
	if res.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("If-Match: * got %d, want 428", res.StatusCode)
	}
}

func TestUpdateAcceptsQuotedAndBareVersions(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	n := createNote(t, alice, "note")

	alice.Patch("/v1/notes/"+n.ID.String(), map[string]any{"body": "one"},
		"If-Match", `"1"`).Require(http.StatusOK)
	alice.Patch("/v1/notes/"+n.ID.String(), map[string]any{"body": "two"},
		"If-Match", "2").Require(http.StatusOK)
}

// A viewer may read but not write. This is the role boundary that a 403 is
// correct for: the caller can already see the note, so its existence is not a
// secret from them.
func TestViewerCanReadButNotUpdate(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	n := createNote(t, alice, "read-only for bob")
	shareWith(t, env, n.ID, alice, bob, "viewer")

	var got note
	bob.Get("/v1/notes/" + n.ID.String()).Require(http.StatusOK).JSON(&got)
	if got.YourRole != "viewer" {
		t.Errorf("your_role = %q, want viewer", got.YourRole)
	}

	if code := bob.Patch("/v1/notes/"+n.ID.String(),
		map[string]any{"body": "nope"}, "If-Match", "1").StatusCode; code != http.StatusForbidden {
		t.Fatalf("viewer update got %d, want 403", code)
	}
}

// Editors deliberately cannot delete: delete is owner-only, and so is restore.
func TestEditorCannotDeleteOrShare(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	n := createNote(t, alice, "bob may edit")
	shareWith(t, env, n.ID, alice, bob, "editor")

	if code := bob.Delete("/v1/notes/" + n.ID.String()).StatusCode; code != http.StatusForbidden {
		t.Errorf("editor delete got %d, want 403", code)
	}
	if code := bob.Post("/v1/notes/"+n.ID.String()+"/restore", nil).StatusCode; code != http.StatusForbidden {
		t.Errorf("editor restore got %d, want 403", code)
	}
}

// A deleted note disappears for everyone it was shared with, while the owner
// can still find and restore it.
func TestSoftDeleteHidesFromSharedUsersAndOwnerCanRestore(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	n := createNote(t, alice, "temporary")
	shareWith(t, env, n.ID, alice, bob, "editor")
	bob.Get("/v1/notes/" + n.ID.String()).Require(http.StatusOK)

	alice.Delete("/v1/notes/" + n.ID.String()).Require(http.StatusNoContent)

	if code := bob.Get("/v1/notes/" + n.ID.String()).StatusCode; code != http.StatusNotFound {
		t.Errorf("shared user sees deleted note: %d", code)
	}
	if code := alice.Get("/v1/notes/" + n.ID.String()).StatusCode; code != http.StatusNotFound {
		t.Errorf("owner GET of a deleted note = %d, want 404", code)
	}

	// Restore is owner-only, and finds the note despite the read path hiding it.
	var restored note
	alice.Post("/v1/notes/"+n.ID.String()+"/restore", nil).
		Require(http.StatusOK).JSON(&restored)

	bob.Get("/v1/notes/" + n.ID.String()).Require(http.StatusOK)
}

func TestRestoringALiveNoteIsAConflict(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	n := createNote(t, alice, "alive")

	if code := alice.Post("/v1/notes/"+n.ID.String()+"/restore", nil).StatusCode; code != http.StatusConflict {
		t.Fatalf("restoring a live note got %d, want 409", code)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	n := createNote(t, alice, "delete twice")

	alice.Delete("/v1/notes/" + n.ID.String()).Require(http.StatusNoContent)
	// The second delete resolves as 404, because a deleted note reads as
	// absent for every action except restore.
	if code := alice.Delete("/v1/notes/" + n.ID.String()).StatusCode; code != http.StatusNotFound {
		t.Fatalf("second delete got %d, want 404", code)
	}
}

// Clearing tags must actually clear them. A nil slice reaching coalesce() in
// the update statement would make "remove all tags" mean "leave unchanged".
func TestTagsCanBeClearedNotJustReplaced(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	var n note
	alice.Post("/v1/notes", map[string]any{"title": "tagged", "tags": []string{"a", "b"}}).
		Require(http.StatusCreated).JSON(&n)

	var cleared note
	alice.Patch("/v1/notes/"+n.ID.String(), map[string]any{"tags": []string{}},
		"If-Match", "1").Require(http.StatusOK).JSON(&cleared)

	if len(cleared.Tags) != 0 {
		t.Fatalf("tags = %v, want empty -- clearing silently did nothing", cleared.Tags)
	}
}

func TestValidationRejectsEmptyAndOverlongTitles(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	for name, title := range map[string]string{
		"empty":      "",
		"whitespace": "   ",
		"overlong":   strings.Repeat("x", 513),
	} {
		t.Run(name, func(t *testing.T) {
			if code := alice.Post("/v1/notes", map[string]any{"title": title}).StatusCode; code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", code)
			}
		})
	}
}

func TestUnauthenticatedAccessIsRejected(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	n := createNote(t, alice, "private")

	anon := env.Anonymous()

	if code := anon.Get("/v1/notes/" + n.ID.String()).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("unauthenticated read = %d, want 401", code)
	}
	if code := anon.Post("/v1/notes", map[string]any{"title": "x"}).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("unauthenticated create = %d, want 401", code)
	}
	if code := anon.Delete("/v1/notes/" + n.ID.String()).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("unauthenticated delete = %d, want 401", code)
	}

	// Worth knowing: chi resolves method-not-allowed outside the group's
	// middleware, so a request to an existing path with an unrouted method
	// returns 405 before authentication runs. That discloses only which
	// methods a documented endpoint supports, so it is acceptable -- but it
	// means a 405 is never proof that a route is unauthenticated.
}

// shareWith writes the grant directly, since the sharing endpoints land in a
// later commit. The rows are what the access query reads either way.
func shareWith(t *testing.T, env *apitest.Env, noteID uuid.UUID, owner, target *apitest.Client, role string) {
	t.Helper()
	_, err := env.Store.Pool.Exec(context.Background(), `
		insert into note_shares (note_id, principal_type, principal_id, role, granted_by)
		values ($1, 'user', $2, $3, $4)
		on conflict (note_id, principal_type, principal_id) do update set role = excluded.role
	`, noteID, target.ID, role, owner.ID)
	if err != nil {
		t.Fatalf("granting %s to %s: %v", role, target.Name, err)
	}
}
