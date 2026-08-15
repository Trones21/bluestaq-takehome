package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The distinction under test: a request that ran out of time and a client that
// hung up are both "the handler returned a context error", but neither is a
// server fault. Left unclassified they land in the 500 bucket, which is the
// one an on-call alert watches -- so overload and disconnects would page
// someone with "internal error" and no way to tell them apart from a real bug.

func TestDeadlineExceededIsRetryable503(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)

	// Wrapped, because pgx returns the context error inside its own.
	WriteProblem(rec, r, fmt.Errorf("querying notes: %w", context.DeadlineExceeded))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("timeouts must keep the problem+json contract, got %q", ct)
	}

	var p Problem
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if p.Status != http.StatusServiceUnavailable {
		t.Fatalf("body status %d disagrees with the header", p.Status)
	}
	if p.Detail == "" {
		t.Fatal("a 503 should tell the client it is worth retrying")
	}
}

func TestClientDisconnectIsNotAServerError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller went away

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil).WithContext(ctx)

	WriteProblem(rec, r, fmt.Errorf("querying notes: %w", context.Canceled))

	if rec.Code != StatusClientClosedRequest {
		t.Fatalf("want 499, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("nobody is listening; want an empty body, got %q", rec.Body.String())
	}
}

func TestCanceledWithLiveRequestStaysAnInternalError(t *testing.T) {
	// A context.Canceled that did not come from *this* request's context is an
	// ordinary bug -- some inner context cancelled early. It must not be
	// laundered into a 499 just because of the error type.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)

	WriteProblem(rec, r, fmt.Errorf("inner helper: %w", context.Canceled))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

func TestProblemErrorsPassThroughUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/notes/x", nil)

	WriteProblem(rec, r, NotFound("note"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestInternalErrorDetailIsNeverSentToTheClient(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)

	WriteProblem(rec, r, errors.New("pq: relation \"notes\" does not exist"))

	var p Problem
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if p.Detail != "an internal error occurred" {
		t.Fatalf("internal detail leaked to the client: %q", p.Detail)
	}
}
