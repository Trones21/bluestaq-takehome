package notes

import (
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/authz"
	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/Trones21/bluestaq-takehome/internal/storage"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// attachmentRef matches the stable reference form used inside note bodies:
//
//	![architecture diagram](attachment:01H8XG5K...)
//
// A presigned URL must NEVER be written into a body. It expires, so the note
// would rot -- content that renders today and 404s in fifteen minutes. The
// body carries this reference and the server resolves it at read time, which
// decouples URL lifetime from content lifetime.
var attachmentRef = regexp.MustCompile(`attachment:([0-9a-fA-F-]{36})`)

type Attachment struct {
	ID        uuid.UUID  `json:"id"`
	Filename  string     `json:"filename"`
	MimeType  string     `json:"mime_type"`
	SizeBytes int64      `json:"size_bytes"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`

	// Fresh on every read, never persisted into the note body.
	DownloadURL string `json:"download_url,omitempty"`
}

type createAttachmentRequest struct {
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
}

type createAttachmentResponse struct {
	Attachment
	UploadURL string `json:"upload_url"`
	// Spelled out because the two-phase flow is not guessable from the payload.
	Instructions string `json:"instructions"`
}

// createAttachment reserves a row and hands back a presigned upload URL.
//
// Bytes never pass through this API. Proxying them would put large uploads on
// the request path of the app server, hold a connection and a database
// transaction for the duration, and rule out a CDN entirely -- the pattern
// that does not survive a production video upload.
func (h *Handler) createAttachment(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionAddAttachment)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	var req createAttachmentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	req.Filename = sanitizeFilename(req.Filename)
	fields := map[string]string{}
	if req.Filename == "" {
		fields["filename"] = "is required"
	}
	if req.SizeBytes <= 0 {
		fields["size_bytes"] = "must be a positive byte count"
	}
	if req.SizeBytes > h.Storage.MaxBytes {
		fields["size_bytes"] = fmt.Sprintf("exceeds the %d byte limit", h.Storage.MaxBytes)
	}
	if !h.allowedMIME(req.MimeType) {
		fields["mime_type"] = fmt.Sprintf("must be one of: %s",
			strings.Join(h.Storage.AllowedMIME, ", "))
	}
	if len(fields) > 0 {
		httpx.WriteProblem(w, r, httpx.Invalid(fields))
		return
	}

	// Namespaced by note so the bucket is browsable, and suffixed with a fresh
	// UUID so two uploads of the same filename never collide.
	objectKey := fmt.Sprintf("notes/%s/%s%s", row.ID, uuid.NewString(), filepath.Ext(req.Filename))

	att, err := h.Store.CreateAttachment(r.Context(), sqlcgen.CreateAttachmentParams{
		NoteID:     row.ID,
		ObjectKey:  objectKey,
		Filename:   req.Filename,
		MimeType:   req.MimeType,
		SizeBytes:  req.SizeBytes,
		UploadedBy: httpx.MustUserID(r.Context()),
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	uploadURL, err := h.Storage.Store.PresignPut(r.Context(), objectKey, h.Storage.UploadTTL)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	h.Metrics.Presigns.WithLabelValues("put").Inc()

	httpx.WriteJSON(w, http.StatusCreated, createAttachmentResponse{
		Attachment: toAttachment(sqlcgen.Attachment(att), ""),
		UploadURL:  uploadURL,
		Instructions: fmt.Sprintf(
			"PUT the file bytes to upload_url within %s, then POST to "+
				"/v1/notes/%s/attachments/%s/complete", h.Storage.UploadTTL, row.ID, att.ID),
	})
}

// completeAttachment verifies the upload actually happened and matches.
//
// This is where the size limit is genuinely enforced. A presigned PUT signs
// the method, key and expiry -- not the body length -- so a client holding
// that URL can upload any number of bytes. A size check that lives only in the
// presign handler is decorative. Presigned POST with a content-length-range
// policy would let S3 reject the upload itself, and is the upgrade.
func (h *Handler) completeAttachment(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionAddAttachment)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	att, err := h.loadAttachment(r, row.ID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if att.Status == "ready" {
		// Idempotent: a retried completion is not an error.
		httpx.WriteJSON(w, http.StatusOK, toAttachment(att, ""))
		return
	}

	info, err := h.Storage.Store.Stat(r.Context(), att.ObjectKey)
	if err != nil {
		if storage.IsNotFound(err) {
			httpx.WriteProblem(w, r, httpx.Errorf(http.StatusConflict,
				"no object was uploaded; PUT the file to the upload_url first"))
			return
		}
		httpx.WriteProblem(w, r, err)
		return
	}

	// The real enforcement point. Over the cap means the object is deleted and
	// the row abandoned: the bytes were already paid for, but they do not stay.
	if info.Size > h.Storage.MaxBytes {
		_ = h.Storage.Store.Delete(r.Context(), att.ObjectKey)
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge,
			"uploaded %d bytes, which exceeds the %d byte limit; the object was discarded",
			info.Size, h.Storage.MaxBytes))
		return
	}

	// size_bytes is recorded from what was actually stored, not from what the
	// client declared, so the number in the database is always true.
	ready, err := h.Store.MarkAttachmentReady(r.Context(), sqlcgen.MarkAttachmentReadyParams{
		ID:        att.ID,
		SizeBytes: info.Size,
		Checksum:  &info.ETag,
	})
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAttachment(sqlcgen.Attachment(ready), ""))
}

// listAttachments returns ready attachments with fresh download URLs.
func (h *Handler) listAttachments(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionDownloadAttachment)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, err := h.Store.ListNoteAttachments(r.Context(), row.ID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]Attachment, 0, len(rows))
	for _, a := range rows {
		url, err := h.Storage.Store.PresignGet(
			r.Context(), a.ObjectKey, a.Filename, h.Storage.DownloadTTL)
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
		h.Metrics.Presigns.WithLabelValues("get").Inc()
		out = append(out, toAttachment(sqlcgen.Attachment(a), url))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"attachments": out})
}

// deleteAttachment removes an attachment, refusing while the body still points
// at it.
//
// Allowing it would leave a broken image in saved content, making the service
// complicit in corrupting its own data. The caller is told where the
// references are so they can remove them first.
func (h *Handler) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	row, _, err := h.load(r, authz.ActionRemoveAttachment)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	att, err := h.loadAttachment(r, row.ID)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if refs := referencedIDs(row.Body); refs[att.ID] {
		httpx.WriteProblem(w, r, httpx.Errorf(http.StatusConflict,
			"the note body still references this attachment (attachment:%s); "+
				"remove the reference before deleting it", att.ID))
		return
	}

	if _, err := h.Store.DeleteAttachment(r.Context(), sqlcgen.DeleteAttachmentParams{
		ID: att.ID, NoteID: row.ID,
	}); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// Row first, object second. The reverse order can leave a row pointing at
	// nothing if the delete fails halfway; this way the worst case is an
	// unreferenced object, which the sweep collects.
	if err := h.Storage.Store.Delete(r.Context(), att.ObjectKey); err != nil {
		httpx.Logger(r).Warn("object left behind after row deletion",
			"object_key", att.ObjectKey, "err", err)
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

func (h *Handler) loadAttachment(r *http.Request, noteID uuid.UUID) (sqlcgen.Attachment, error) {
	id, err := uuid.Parse(chi.URLParam(r, "attachmentID"))
	if err != nil {
		return sqlcgen.Attachment{}, httpx.NotFound("attachment")
	}
	att, err := h.Store.GetAttachment(r.Context(), sqlcgen.GetAttachmentParams{
		ID: id, NoteID: noteID,
	})
	if err != nil {
		if store.IsNotFound(err) {
			return sqlcgen.Attachment{}, httpx.NotFound("attachment")
		}
		return sqlcgen.Attachment{}, err
	}
	return sqlcgen.Attachment(att), nil
}

// validateBodyReferences checks every attachment: reference in a body.
//
// Without this you accumulate dangling references, and the cross-note case is
// real: copy body text between notes and the reference points at another
// note's attachment. Rejected rather than silently resolved -- auto-copying
// attachments across notes is a feature, not a bug fix.
func (h *Handler) validateBodyReferences(r *http.Request, noteID uuid.UUID, body string) error {
	refs := referencedIDs(body)
	if len(refs) == 0 {
		return nil
	}

	bad := map[string]string{}
	for id := range refs {
		att, err := h.Store.GetAttachment(r.Context(), sqlcgen.GetAttachmentParams{
			ID: id, NoteID: noteID,
		})
		if err != nil {
			if store.IsNotFound(err) {
				bad[id.String()] = "no such attachment on this note"
				continue
			}
			return err
		}
		if att.Status != "ready" {
			bad[id.String()] = "upload has not been completed"
		}
	}
	if len(bad) > 0 {
		p := httpx.Errorf(http.StatusBadRequest,
			"the body references attachments that are not usable on this note")
		p.Errors = bad
		return p
	}
	return nil
}

// referencedIDs extracts the attachment ids a body points at. The body IS the
// position -- no separate ordering column is needed, because Markdown already
// says where each image renders.
func referencedIDs(body string) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, m := range attachmentRef.FindAllStringSubmatch(body, -1) {
		if id, err := uuid.Parse(m[1]); err == nil {
			out[id] = true
		}
	}
	return out
}

func (h *Handler) allowedMIME(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	for _, allowed := range h.Storage.AllowedMIME {
		// Prefixes so "image/" covers the whole family, exact matches
		// otherwise.
		if strings.HasSuffix(allowed, "/") {
			if strings.HasPrefix(mime, allowed) {
				return true
			}
		} else if mime == allowed {
			return true
		}
	}
	return false
}

// sanitizeFilename keeps the base name only.
//
// The filename is echoed in a Content-Disposition header, so a path or a
// newline in it is a header-injection and path-traversal vector. It never
// reaches the object key, which is generated.
func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\\", "/")
	s = filepath.Base(s)
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '"' {
			return -1
		}
		return r
	}, s)
	if s == "." || s == ".." || s == "/" {
		return ""
	}
	if len(s) > 255 {
		s = s[:255]
	}
	return s
}

func toAttachment(a sqlcgen.Attachment, downloadURL string) Attachment {
	return Attachment{
		ID: a.ID, Filename: a.Filename, MimeType: a.MimeType,
		SizeBytes: a.SizeBytes, Status: a.Status,
		CreatedAt: a.CreatedAt, ReadyAt: a.ReadyAt,
		DownloadURL: downloadURL,
	}
}
