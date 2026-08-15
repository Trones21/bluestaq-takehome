// Package apitest runs the real router against a real database over real HTTP.
//
// Nothing is stubbed but the clock. The authorization rules only mean anything
// with several users acting against each other, and that is exactly what a
// single-client handler test cannot express.
package apitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/auth"
	"github.com/Trones21/bluestaq-takehome/internal/config"
	"github.com/Trones21/bluestaq-takehome/internal/notes"
	"github.com/Trones21/bluestaq-takehome/internal/obs"
	"github.com/Trones21/bluestaq-takehome/internal/server"
	"github.com/Trones21/bluestaq-takehome/internal/storage"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/teams"
	"github.com/Trones21/bluestaq-takehome/internal/testdb"
	"github.com/Trones21/bluestaq-takehome/internal/users"
	"github.com/google/uuid"
)

// Password used by every fixture account. Long enough to pass validation.
const Password = "fixture-password-42"

type Env struct {
	T       *testing.T
	Store   *store.Store
	Server  *httptest.Server
	Metrics *obs.Metrics
}

func New(t *testing.T) *Env {
	t.Helper()
	st := testdb.New(t)

	metrics := obs.NewMetrics()
	tokens := auth.NewTokens([]byte(strings.Repeat("t", 32)), time.Hour)

	// Real MinIO from docker-compose, not a fake. The interesting failures in
	// the attachment flow are what object storage actually does -- a missing
	// object at completion, a size that does not match what was declared --
	// and a stub would assert only that we called our own interface.
	storeCfg := config.StorageConfig{
		Endpoint:     envOr("TEST_S3_ENDPOINT", "http://localhost:9000"),
		Region:       "us-east-1",
		Bucket:       envOr("TEST_S3_BUCKET", "notes-attachments"),
		AccessKey:    envOr("TEST_S3_ACCESS_KEY", "minioadmin"),
		SecretKey:    envOr("TEST_S3_SECRET_KEY", "minioadmin"),
		UsePathStyle: true,
		UploadTTL:    15 * time.Minute,
		DownloadTTL:  time.Hour,
		// Small on purpose, so the over-limit path is testable without moving
		// a hundred megabytes through the test.
		MaxAttachmentBytes: 1 << 20,
		AllowedMIMEPrefix:  []string{"image/", "video/mp4", "application/pdf"},
	}
	objects, err := storage.New(storeCfg)
	if err != nil {
		t.Fatalf("object storage: %v", err)
	}

	deps := server.Deps{
		Config:  &config.Config{},
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Metrics: metrics,
		DB:      st.Pool,
		Tokens:  tokens,
		Users:   users.New(st, tokens),
		Notes: notes.New(st, metrics, notes.StorageDeps{
			Store:       objects,
			UploadTTL:   storeCfg.UploadTTL,
			DownloadTTL: storeCfg.DownloadTTL,
			MaxBytes:    storeCfg.MaxAttachmentBytes,
			AllowedMIME: storeCfg.AllowedMIMEPrefix,
		}),
		Teams: teams.New(st),
	}

	srv := httptest.NewServer(server.PublicRouter(deps))
	t.Cleanup(srv.Close)

	return &Env{T: t, Store: st, Server: srv, Metrics: metrics}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Client is one authenticated user's view of the API.
type Client struct {
	env         *Env
	Name        string
	Email       string
	Token       string
	ID          uuid.UUID
	http        *http.Client
	DisplayName string
}

// Register creates an account and returns a client authenticated as them.
func (e *Env) Register(name string) *Client {
	e.T.Helper()

	email := fmt.Sprintf("%s-%s@example.test", name, uuid.NewString()[:8])
	c := &Client{env: e, Name: name, Email: email, DisplayName: name, http: e.Server.Client()}

	var out struct {
		AccessToken string `json:"access_token"`
		User        struct {
			ID uuid.UUID `json:"id"`
		} `json:"user"`
	}
	res := c.Post("/v1/auth/register", map[string]string{
		"email":        email,
		"password":     Password,
		"display_name": name,
	})
	res.Require(http.StatusOK).JSON(&out)

	c.Token = out.AccessToken
	c.ID = out.User.ID
	return c
}

// Anonymous returns a client with no credentials.
func (e *Env) Anonymous() *Client {
	return &Client{env: e, Name: "anonymous", http: e.Server.Client()}
}

type Response struct {
	t          *testing.T
	StatusCode int
	Header     http.Header
	Body       []byte
	request    string
}

// Require fails the test unless the status matches, printing the body -- which
// is where the problem+json detail lives.
func (r *Response) Require(want int) *Response {
	r.t.Helper()
	if r.StatusCode != want {
		r.t.Fatalf("%s\n  got status %d, want %d\n  body: %s",
			r.request, r.StatusCode, want, strings.TrimSpace(string(r.Body)))
	}
	return r
}

func (r *Response) JSON(dst any) *Response {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, dst); err != nil {
		r.t.Fatalf("%s\n  body is not JSON: %v\n  body: %s", r.request, err, r.Body)
	}
	return r
}

// ETag returns the entity tag with its quotes stripped, ready to send back as
// If-Match.
func (r *Response) ETag() string {
	return strings.Trim(r.Header.Get("ETag"), `"`)
}

// Detail returns the problem+json detail message.
func (r *Response) Detail() string {
	var p struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(r.Body, &p)
	return p.Detail
}

func (c *Client) Get(path string) *Response    { return c.Do(http.MethodGet, path, nil) }
func (c *Client) Delete(path string) *Response { return c.Do(http.MethodDelete, path, nil) }

func (c *Client) Post(path string, body any) *Response {
	return c.Do(http.MethodPost, path, body)
}

func (c *Client) Patch(path string, body any, headers ...string) *Response {
	return c.Do(http.MethodPatch, path, body, headers...)
}

// Do issues a request as this user. Extra headers are passed as alternating
// key/value strings, which keeps If-Match at the call site readable.
func (c *Client) Do(method, path string, body any, headers ...string) *Response {
	c.env.T.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.env.T.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.env.Server.URL+path, reader)
	if err != nil {
		c.env.T.Fatalf("building request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	res, err := c.http.Do(req)
	if err != nil {
		c.env.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		c.env.T.Fatalf("reading response: %v", err)
	}

	return &Response{
		t:          c.env.T,
		StatusCode: res.StatusCode,
		Header:     res.Header,
		Body:       raw,
		request:    fmt.Sprintf("%s %s (as %s)", method, path, c.Name),
	}
}

// IfMatch is sugar for the header pair, so calls read as the scenario.
func IfMatch(version string) []string { return []string{"If-Match", version} }
