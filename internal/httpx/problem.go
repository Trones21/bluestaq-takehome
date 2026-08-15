// Package httpx holds the HTTP plumbing shared by every handler: RFC 9457
// problem responses, JSON encode/decode, and request-scoped values.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/Trones21/bluestaq-takehome/internal/config"
)

// StatusClientClosedRequest is nginx's non-standard 499. Nothing is sent to the
// client -- by definition they are gone -- but recording it keeps a disconnect
// out of the 5xx bucket, where it would look like a server fault.
const StatusClientClosedRequest = 499

// Problem is an RFC 9457 "problem details" response body. One error shape for
// the whole API means clients write one error handler.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`

	// Errors carries field-level validation failures. Present only on 400.
	Errors map[string]string `json:"errors,omitempty"`
	// Conflict carries the current representation on a 409 version conflict,
	// so the client can show a diff instead of just an error. See SPEC 5.
	Conflict any `json:"conflict,omitempty"`
}

func (p *Problem) Error() string {
	return fmt.Sprintf("%d %s: %s", p.Status, p.Title, p.Detail)
}

// Errorf builds a Problem with a formatted detail message.
func Errorf(status int, format string, args ...any) *Problem {
	return &Problem{
		Type:   problemType(status),
		Title:  http.StatusText(status),
		Status: status,
		Detail: fmt.Sprintf(format, args...),
	}
}

// NotFound is the response for both "does not exist" and "you may not read
// this". Returning 403 for the second case would confirm the resource exists,
// turning ID enumeration into a way to learn about other teams' data.
// See DISCUSSION 8.
func NotFound(resource string) *Problem {
	return Errorf(http.StatusNotFound, "%s not found", resource)
}

// Forbidden is used only when the caller can already see the resource but
// lacks the role for this particular action -- existence is not a secret from
// them, so there is nothing left to leak.
func Forbidden(action string) *Problem {
	return Errorf(http.StatusForbidden, "you do not have permission to %s", action)
}

func Invalid(fields map[string]string) *Problem {
	p := Errorf(http.StatusBadRequest, "the request body failed validation")
	p.Errors = fields
	return p
}

func problemType(status int) string {
	// RFC 9457 wants a URI that documents the error class. Pointing at the
	// relevant RFC section beats inventing URLs that resolve to nothing.
	switch {
	case status >= 500:
		return "https://datatracker.ietf.org/doc/html/rfc9110#section-15.6"
	default:
		return "https://datatracker.ietf.org/doc/html/rfc9110#section-15.5"
	}
}

// WriteProblem renders err as problem+json. Anything that is not already a
// *Problem is treated as an internal error: the detail is logged, never sent,
// because internal error strings leak schema and file paths.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	// Context errors arrive here as raw errors from pgx or the S3 client, and
	// neither is an internal fault. Classifying them before the generic 500
	// path keeps two very different events out of the error log -- shed load,
	// and a client that hung up -- so a 500 keeps meaning "this service has a
	// bug", which is what an alert on it should mean.
	//
	// A disconnect leaves no one to write to, so it records a status for the
	// log and the metric and sends no body.
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		Logger(r).Info("client disconnected before the response was written")
		w.WriteHeader(StatusClientClosedRequest)
		return
	}

	var p *Problem
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		Logger(r).Warn("request exceeded its deadline", slog.Any("err", err))
		p = Errorf(http.StatusServiceUnavailable,
			"the server took too long to handle this request; retry shortly")
	case errors.As(err, &p):
		if p.Status >= 500 {
			Logger(r).Error("request failed", slog.Any("err", err), slog.Int("status", p.Status))
		}
	default:
		Logger(r).Error("unhandled error", slog.Any("err", err))
		p = Errorf(http.StatusInternalServerError, "an internal error occurred")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON renders v as JSON with the given status. A nil body writes just
// the status, for 204s.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	if v == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// DecodeJSON reads a JSON body into dst with a size cap and strict field
// checking. Unknown fields are rejected so a client typo silently dropping a
// field surfaces as an error instead of a mysteriously ignored update.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return Errorf(http.StatusRequestEntityTooLarge,
				"request body exceeds %d bytes", config.MaxRequestBytes)
		case errors.Is(err, io.EOF):
			return Errorf(http.StatusBadRequest, "a request body is required")
		default:
			return Errorf(http.StatusBadRequest, "malformed JSON: %v", err)
		}
	}
	// A second value in the stream means the client sent something we would
	// silently ignore.
	if dec.More() {
		return Errorf(http.StatusBadRequest, "request body must contain a single JSON object")
	}
	return nil
}
