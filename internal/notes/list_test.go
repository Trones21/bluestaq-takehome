package notes_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/Trones21/bluestaq-takehome/internal/apitest"
	"github.com/google/uuid"
)

type listResponse struct {
	Notes []struct {
		ID              uuid.UUID `json:"id"`
		Title           string    `json:"title"`
		Tags            []string  `json:"tags"`
		YourRole        string    `json:"your_role"`
		AttachmentCount int64     `json:"attachment_count"`
	} `json:"notes"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

func list(t *testing.T, c *apitest.Client, query string) listResponse {
	t.Helper()
	var out listResponse
	c.Get("/v1/notes" + query).Require(http.StatusOK).JSON(&out)
	return out
}

func titles(res listResponse) []string {
	out := make([]string, 0, len(res.Notes))
	for _, n := range res.Notes {
		out = append(out, n.Title)
	}
	return out
}

// The rule that makes every other filter safe: search is a filter ON the
// authorized set, so it can never surface a note the caller cannot read.
func TestSearchCannotSurfaceUnreadableNotes(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	alice.Post("/v1/notes", map[string]any{
		"title": "Postgres indexing", "body": "vacuum and bloat",
	}).Require(http.StatusCreated)

	// Bob's own note matches the same term, so a leak is distinguishable from
	// an empty result.
	bob.Post("/v1/notes", map[string]any{
		"title": "Bob on indexing", "body": "his own note",
	}).Require(http.StatusCreated)

	res := list(t, bob, "?q=indexing")
	if got := titles(res); len(got) != 1 || got[0] != "Bob on indexing" {
		t.Fatalf("search returned %v -- it must not reach Alice's private note", got)
	}
}

func TestFullTextSearchMatchesTitleAndBody(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	for _, n := range []map[string]any{
		{"title": "Postgres indexing", "body": "about vacuum"},
		{"title": "Meeting notes", "body": "we discussed indexing strategy"},
		{"title": "Grocery list", "body": "milk and eggs"},
	} {
		alice.Post("/v1/notes", n).Require(http.StatusCreated)
	}

	res := list(t, alice, "?q=indexing")
	if len(res.Notes) != 2 {
		t.Fatalf("q=indexing returned %v, want the two indexing notes", titles(res))
	}
	// Title is weight A and body weight B, so the title match ranks first.
	if res.Notes[0].Title != "Postgres indexing" {
		t.Errorf("ranked %v first; a title match should outrank a body match", res.Notes[0].Title)
	}

	// Stemming: "discuss" must find "discussed".
	if got := list(t, alice, "?q=discuss"); len(got.Notes) != 1 {
		t.Errorf("q=discuss returned %v, want the stemmed match", titles(got))
	}
}

// websearch_to_tsquery is used precisely so that whatever a user types is
// treated as a query rather than raising a syntax error.
func TestSearchAcceptsWhateverUsersType(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	alice.Post("/v1/notes", map[string]any{"title": "Quarterly plan", "body": "x"}).
		Require(http.StatusCreated)

	for _, q := range []string{`"quarterly plan"`, "quarterly OR nonsense", "quarterly -zzz", "&&&", "a & b | c"} {
		res := alice.Get("/v1/notes?q=" + url.QueryEscape(q))
		if res.StatusCode != http.StatusOK {
			t.Errorf("q=%q returned %d; user input must never be a 500", q, res.StatusCode)
		}
	}
}

func TestTagFilterIsAndNotOr(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	alice.Post("/v1/notes", map[string]any{"title": "both", "tags": []string{"db", "perf"}}).
		Require(http.StatusCreated)
	alice.Post("/v1/notes", map[string]any{"title": "only db", "tags": []string{"db"}}).
		Require(http.StatusCreated)

	if got := titles(list(t, alice, "?tag=db")); len(got) != 2 {
		t.Fatalf("tag=db returned %v, want both", got)
	}
	got := titles(list(t, alice, "?tag=db&tag=perf"))
	if len(got) != 1 || got[0] != "both" {
		t.Fatalf("tag=db&tag=perf returned %v -- repeated tags must AND, not OR", got)
	}
}

func TestOwnerAndSharedWithMeFilters(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	var aliceNote struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/notes", map[string]any{"title": "from alice"}).
		Require(http.StatusCreated).JSON(&aliceNote)
	bob.Post("/v1/notes", map[string]any{"title": "bob's own"}).Require(http.StatusCreated)

	alice.Do(http.MethodPut, "/v1/notes/"+aliceNote.ID.String()+"/shares", map[string]any{
		"principal_type": "user", "principal_id": bob.ID, "role": "viewer",
	}).Require(http.StatusNoContent)

	// Bob sees both, one owned and one shared.
	if got := titles(list(t, bob, "")); len(got) != 2 {
		t.Fatalf("bob sees %v, want both", got)
	}
	if got := titles(list(t, bob, "?shared_with_me=true")); len(got) != 1 || got[0] != "from alice" {
		t.Fatalf("shared_with_me returned %v", got)
	}
	if got := titles(list(t, bob, "?owner=me")); len(got) != 1 || got[0] != "bob's own" {
		t.Fatalf("owner=me returned %v", got)
	}

	// The invariant: ?owner=alice returns Alice's notes Bob can see, not all
	// of Alice's notes.
	alice.Post("/v1/notes", map[string]any{"title": "alice private"}).Require(http.StatusCreated)
	got := titles(list(t, bob, "?owner="+alice.ID.String()))
	if len(got) != 1 || got[0] != "from alice" {
		t.Fatalf("owner filter returned %v -- it must narrow, never widen", got)
	}
}

func TestKeysetPaginationWalksEveryNoteExactlyOnce(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	const total = 12
	for i := 0; i < total; i++ {
		alice.Post("/v1/notes", map[string]any{"title": fmt.Sprintf("note-%02d", i)}).
			Require(http.StatusCreated)
	}

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		q := "?limit=5"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		res := list(t, alice, q)
		for _, n := range res.Notes {
			seen[n.Title]++
		}
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}

	if len(seen) != total {
		t.Fatalf("walked %d distinct notes, want %d", len(seen), total)
	}
	for title, count := range seen {
		if count != 1 {
			t.Errorf("%s appeared %d times -- pagination duplicated a row", title, count)
		}
	}
}

func TestDeletedNotesAreOwnerScoped(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	var note struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/notes", map[string]any{"title": "doomed"}).
		Require(http.StatusCreated).JSON(&note)
	alice.Do(http.MethodPut, "/v1/notes/"+note.ID.String()+"/shares", map[string]any{
		"principal_type": "user", "principal_id": bob.ID, "role": "editor",
	}).Require(http.StatusNoContent)

	alice.Delete("/v1/notes/" + note.ID.String()).Require(http.StatusNoContent)

	if got := titles(list(t, alice, "?deleted=true")); len(got) != 1 {
		t.Fatalf("owner cannot see their own deleted note: %v", got)
	}
	// Bob had editor access, but a deleted note is gone for everyone but its
	// owner -- ?deleted=true must not become a back door to it.
	if got := titles(list(t, bob, "?deleted=true")); len(got) != 0 {
		t.Fatalf("shared user sees the owner's deleted note: %v", got)
	}
	if got := titles(list(t, alice, "")); len(got) != 0 {
		t.Fatalf("deleted note appears in the default list: %v", got)
	}
}

func TestTeamFilterRequiresMembership(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	outsider := env.Register("outsider")

	var team struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/teams", map[string]string{"name": "Platform"}).
		Require(http.StatusCreated).JSON(&team)

	var note struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/notes", map[string]any{"title": "team note"}).
		Require(http.StatusCreated).JSON(&note)
	alice.Do(http.MethodPut, "/v1/notes/"+note.ID.String()+"/shares", map[string]any{
		"principal_type": "team", "principal_id": team.ID, "role": "viewer",
	}).Require(http.StatusNoContent)

	if got := titles(list(t, alice, "?team="+team.ID.String())); len(got) != 1 {
		t.Fatalf("member sees %v, want the team note", got)
	}
	// An outsider filtering by a team id must learn nothing -- otherwise the
	// endpoint reports whether an arbitrary team has notes.
	if got := titles(list(t, outsider, "?team="+team.ID.String())); len(got) != 0 {
		t.Fatalf("outsider sees %v via a team filter", got)
	}
}

func TestListRejectsBadParameters(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	for name, q := range map[string]string{
		"bad cursor":    "?cursor=not-base64!!",
		"bad owner":     "?owner=nope",
		"bad team":      "?team=nope",
		"zero limit":    "?limit=0",
		"contradiction": "?owner=me&shared_with_me=true",
	} {
		t.Run(name, func(t *testing.T) {
			if code := alice.Get("/v1/notes" + q).StatusCode; code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", code)
			}
		})
	}

	// An oversized limit is clamped rather than refused: the server keeps
	// control of how much work a request costs without failing the caller.
	alice.Get("/v1/notes?limit=100000").Require(http.StatusOK)
}

func TestListReportsAttachmentCountNotURLs(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	var note struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/notes", map[string]any{"title": "with files"}).
		Require(http.StatusCreated).JSON(&note)

	// Written directly: the attachment endpoints land in the next commit, and
	// what matters here is that the list aggregates rather than presigns.
	_, err := env.Store.Pool.Exec(context.Background(), `
		insert into attachments (note_id, object_key, filename, mime_type, size_bytes, status, uploaded_by)
		values ($1, $2, 'diagram.png', 'image/png', 1024, 'ready', $3),
		       ($1, $4, 'pending.png', 'image/png', 1024, 'pending', $3)
	`, note.ID, "k-"+uuid.NewString(), alice.ID, "k-"+uuid.NewString())
	if err != nil {
		t.Fatalf("seeding attachments: %v", err)
	}

	res := list(t, alice, "")
	if len(res.Notes) != 1 {
		t.Fatalf("want 1 note, got %d", len(res.Notes))
	}
	// Only the ready one counts; a pending row is an upload nobody finished.
	if res.Notes[0].AttachmentCount != 1 {
		t.Fatalf("attachment_count = %d, want 1 (pending uploads must not count)",
			res.Notes[0].AttachmentCount)
	}
}
