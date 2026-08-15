package notes_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/Trones21/bluestaq-takehome/internal/apitest"
	"github.com/google/uuid"
)

type attachment struct {
	ID          uuid.UUID `json:"id"`
	Filename    string    `json:"filename"`
	SizeBytes   int64     `json:"size_bytes"`
	Status      string    `json:"status"`
	DownloadURL string    `json:"download_url"`
	UploadURL   string    `json:"upload_url"`
}

// upload PUTs bytes straight to object storage, which is the whole point: they
// never pass through the API.
func upload(t *testing.T, url string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building upload: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("uploading: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func newNote(t *testing.T, c *apitest.Client, title string) uuid.UUID {
	t.Helper()
	var n struct {
		ID uuid.UUID `json:"id"`
	}
	c.Post("/v1/notes", map[string]any{"title": title}).
		Require(http.StatusCreated).JSON(&n)
	return n.ID
}

func presign(t *testing.T, c *apitest.Client, noteID uuid.UUID, filename string, size int64) attachment {
	t.Helper()
	var a attachment
	c.Post("/v1/notes/"+noteID.String()+"/attachments", map[string]any{
		"filename": filename, "mime_type": "image/png", "size_bytes": size,
	}).Require(http.StatusCreated).JSON(&a)
	return a
}

func TestFullUploadCycle(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "with a diagram")

	body := []byte("fake png bytes")
	a := presign(t, alice, noteID, "diagram.png", int64(len(body)))

	if a.Status != "pending" {
		t.Fatalf("status = %q, want pending before upload", a.Status)
	}
	if a.UploadURL == "" {
		t.Fatal("no upload_url issued")
	}
	// It must address object storage directly, not this API.
	if strings.Contains(a.UploadURL, env.Server.URL) {
		t.Fatalf("upload_url points at the API: %s", a.UploadURL)
	}

	if code := upload(t, a.UploadURL, body); code != http.StatusOK {
		t.Fatalf("upload to object storage returned %d", code)
	}

	var ready attachment
	alice.Post("/v1/notes/"+noteID.String()+"/attachments/"+a.ID.String()+"/complete", nil).
		Require(http.StatusOK).JSON(&ready)
	if ready.Status != "ready" {
		t.Fatalf("status = %q after completion", ready.Status)
	}
	if ready.SizeBytes != int64(len(body)) {
		t.Errorf("size = %d, want %d (recorded from the object, not the claim)",
			ready.SizeBytes, len(body))
	}

	var listed struct {
		Attachments []attachment `json:"attachments"`
	}
	alice.Get("/v1/notes/" + noteID.String() + "/attachments").
		Require(http.StatusOK).JSON(&listed)
	if len(listed.Attachments) != 1 || listed.Attachments[0].DownloadURL == "" {
		t.Fatalf("listing returned %+v, want one entry with a download URL", listed.Attachments)
	}
}

// Completing without uploading must not mark the row ready. The presigned URL
// only creates the opportunity to upload; it is not evidence one happened.
func TestCompleteWithoutUploadIsRejected(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "abandoned")

	a := presign(t, alice, noteID, "never-sent.png", 100)
	alice.Post("/v1/notes/"+noteID.String()+"/attachments/"+a.ID.String()+"/complete", nil).
		Require(http.StatusConflict)
}

// The trap this design has to handle: a presigned PUT signs the method, key
// and expiry -- NOT the body length. A client holding the URL can upload
// anything, so the cap is enforced at completion, not at presign.
func TestPresignedPutCannotEnforceSizeSoCompletionDoes(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "oversized")

	// Declare something small and legal.
	a := presign(t, alice, noteID, "small.png", 100)

	// Upload well over the 1 MiB test cap. Object storage accepts it, which is
	// exactly the point being demonstrated.
	oversized := bytes.Repeat([]byte("x"), (1<<20)+1024)
	if code := upload(t, a.UploadURL, oversized); code != http.StatusOK {
		t.Fatalf("object storage refused the oversized upload (%d) -- "+
			"the premise of this test no longer holds", code)
	}

	res := alice.Post("/v1/notes/"+noteID.String()+"/attachments/"+a.ID.String()+"/complete", nil)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("completion returned %d, want 413 -- the size cap is not enforced anywhere",
			res.StatusCode)
	}
}

func TestPresignValidatesSizeAndMIMEUpFront(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "validation")

	for name, body := range map[string]map[string]any{
		"oversized declaration": {"filename": "big.png", "mime_type": "image/png", "size_bytes": 1 << 30},
		"disallowed mime":       {"filename": "run.sh", "mime_type": "application/x-sh", "size_bytes": 10},
		"zero size":             {"filename": "empty.png", "mime_type": "image/png", "size_bytes": 0},
		"no filename":           {"filename": "", "mime_type": "image/png", "size_bytes": 10},
	} {
		t.Run(name, func(t *testing.T) {
			if code := alice.Post("/v1/notes/"+noteID.String()+"/attachments", body).StatusCode; code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", code)
			}
		})
	}
}

func TestAttachmentPermissionsFollowTheNote(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	viewer := env.Register("viewer")
	stranger := env.Register("stranger")

	noteID := newNote(t, alice, "shared")
	alice.Do(http.MethodPut, "/v1/notes/"+noteID.String()+"/shares", map[string]any{
		"principal_type": "user", "principal_id": viewer.ID, "role": "viewer",
	}).Require(http.StatusNoContent)

	body := []byte("bytes")
	a := presign(t, alice, noteID, "doc.png", int64(len(body)))
	upload(t, a.UploadURL, body)
	alice.Post("/v1/notes/"+noteID.String()+"/attachments/"+a.ID.String()+"/complete", nil).
		Require(http.StatusOK)

	// A viewer may download but not upload -- the same split as the note body.
	viewer.Get("/v1/notes/" + noteID.String() + "/attachments").Require(http.StatusOK)
	if code := viewer.Post("/v1/notes/"+noteID.String()+"/attachments", map[string]any{
		"filename": "x.png", "mime_type": "image/png", "size_bytes": 10,
	}).StatusCode; code != http.StatusForbidden {
		t.Errorf("viewer upload got %d, want 403", code)
	}

	// A stranger learns nothing, including that the note exists.
	if code := stranger.Get("/v1/notes/" + noteID.String() + "/attachments").StatusCode; code != http.StatusNotFound {
		t.Errorf("stranger got %d, want 404", code)
	}
}

// A presigned URL must never be written into a note body -- it expires, and
// the note would rot. The body carries a stable reference instead.
func TestBodyReferencesMustResolveToThisNote(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	noteID := newNote(t, alice, "with image")
	otherID := newNote(t, alice, "other note")

	body := []byte("png")
	a := presign(t, alice, noteID, "diagram.png", int64(len(body)))

	// Still pending: an unfinished upload cannot be referenced.
	alice.Patch("/v1/notes/"+noteID.String(),
		map[string]any{"body": "![d](attachment:" + a.ID.String() + ")"},
		"If-Match", "1").Require(http.StatusBadRequest)

	upload(t, a.UploadURL, body)
	alice.Post("/v1/notes/"+noteID.String()+"/attachments/"+a.ID.String()+"/complete", nil).
		Require(http.StatusOK)

	// Now it resolves.
	alice.Patch("/v1/notes/"+noteID.String(),
		map[string]any{"body": "Here it is:\n\n![d](attachment:" + a.ID.String() + ")"},
		"If-Match", "1").Require(http.StatusOK)

	// The copy-paste case: the same body text in a different note points at an
	// attachment that is not its own.
	alice.Patch("/v1/notes/"+otherID.String(),
		map[string]any{"body": "![d](attachment:" + a.ID.String() + ")"},
		"If-Match", "1").Require(http.StatusBadRequest)

	// A reference to nothing at all.
	alice.Patch("/v1/notes/"+otherID.String(),
		map[string]any{"body": "![d](attachment:" + uuid.NewString() + ")"},
		"If-Match", "1").Require(http.StatusBadRequest)
}

func TestCannotDeleteAnAttachmentTheBodyStillUses(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "with image")

	body := []byte("png")
	a := presign(t, alice, noteID, "diagram.png", int64(len(body)))
	upload(t, a.UploadURL, body)
	alice.Post("/v1/notes/"+noteID.String()+"/attachments/"+a.ID.String()+"/complete", nil).
		Require(http.StatusOK)

	alice.Patch("/v1/notes/"+noteID.String(),
		map[string]any{"body": "![d](attachment:" + a.ID.String() + ")"},
		"If-Match", "1").Require(http.StatusOK)

	// Refused, rather than leaving a broken image in saved content.
	res := alice.Delete("/v1/notes/" + noteID.String() + "/attachments/" + a.ID.String())
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("delete returned %d, want 409", res.StatusCode)
	}
	if !strings.Contains(res.Detail(), a.ID.String()) {
		t.Errorf("the 409 should name the reference to remove: %s", res.Detail())
	}

	// Remove the reference, then it deletes.
	alice.Patch("/v1/notes/"+noteID.String(), map[string]any{"body": "no image now"},
		"If-Match", "2").Require(http.StatusOK)
	alice.Delete("/v1/notes/" + noteID.String() + "/attachments/" + a.ID.String()).
		Require(http.StatusNoContent)
}

// An attached-but-not-inlined file is legitimate, so it must survive. Only
// unfinished uploads are garbage.
func TestUnreferencedAttachmentsAreNotOrphans(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "with a plain attachment")

	body := []byte("pdf bytes")
	a := presign(t, alice, noteID, "report.pdf", int64(len(body)))
	upload(t, a.UploadURL, body)
	alice.Post("/v1/notes/"+noteID.String()+"/attachments/"+a.ID.String()+"/complete", nil).
		Require(http.StatusOK)

	// The body never mentions it, and a plain body edit must not disturb it.
	alice.Patch("/v1/notes/"+noteID.String(), map[string]any{"body": "see attached"},
		"If-Match", "1").Require(http.StatusOK)

	var listed struct {
		Attachments []attachment `json:"attachments"`
	}
	alice.Get("/v1/notes/" + noteID.String() + "/attachments").
		Require(http.StatusOK).JSON(&listed)
	if len(listed.Attachments) != 1 {
		t.Fatalf("unreferenced attachment was dropped: %+v", listed.Attachments)
	}
}

// The filename is echoed in a Content-Disposition header, so a path or a
// control character in it is an injection vector.
func TestFilenameIsSanitized(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "hostile filename")

	var a attachment
	alice.Post("/v1/notes/"+noteID.String()+"/attachments", map[string]any{
		"filename": "../../../etc/passwd.png", "mime_type": "image/png", "size_bytes": 10,
	}).Require(http.StatusCreated).JSON(&a)

	if strings.Contains(a.Filename, "/") || strings.Contains(a.Filename, "..") {
		t.Fatalf("filename kept path components: %q", a.Filename)
	}
	if a.Filename != "passwd.png" {
		t.Errorf("filename = %q, want the base name", a.Filename)
	}
}

func TestCompletionIsIdempotent(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	noteID := newNote(t, alice, "retry")

	body := []byte("bytes")
	a := presign(t, alice, noteID, "f.png", int64(len(body)))
	upload(t, a.UploadURL, body)

	path := "/v1/notes/" + noteID.String() + "/attachments/" + a.ID.String() + "/complete"
	alice.Post(path, nil).Require(http.StatusOK)
	alice.Post(path, nil).Require(http.StatusOK)
}
