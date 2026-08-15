package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/httpx"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var testSecret = []byte(strings.Repeat("s", 32))

func TestTokenRoundTrip(t *testing.T) {
	tk := NewTokens(testSecret, time.Hour)
	want := uuid.New()
	now := time.Now()

	token, expires, err := tk.Issue(want, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !expires.After(now) {
		t.Fatal("expiry is not in the future")
	}

	got, err := tk.Parse(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != want {
		t.Fatalf("subject = %v, want %v", got, want)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	tk := NewTokens(testSecret, time.Hour)
	token, _, err := tk.Issue(uuid.New(), time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := tk.Parse(token); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestTokenSignedWithAnotherKeyRejected(t *testing.T) {
	issued, _, err := NewTokens([]byte(strings.Repeat("x", 32)), time.Hour).Issue(uuid.New(), time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := NewTokens(testSecret, time.Hour).Parse(issued); err == nil {
		t.Fatal("token signed with a different key was accepted")
	}
}

// TestAlgNoneRejected covers the classic JWT bypass: strip the signature and
// declare "alg":"none". A parser that trusts the header's algorithm accepts
// it. jwt.WithValidMethods is what stops this, and this test fails if that
// option is ever dropped.
func TestAlgNoneRejected(t *testing.T) {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	forged := enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." +
		enc(map[string]any{
			"sub": uuid.NewString(),
			"iss": issuer,
			"exp": time.Now().Add(time.Hour).Unix(),
		}) + "."

	if _, err := NewTokens(testSecret, time.Hour).Parse(forged); err == nil {
		t.Fatal("alg=none token accepted -- signature verification is bypassable")
	}
}

// A token from another system that happens to share our signing key must still
// be rejected, which is what validating the issuer buys.
func TestForeignIssuerRejected(t *testing.T) {
	claims := jwt.RegisteredClaims{
		Subject:   uuid.NewString(),
		Issuer:    "some-other-service",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := NewTokens(testSecret, time.Hour).Parse(signed); err == nil {
		t.Fatal("token from a foreign issuer accepted")
	}
}

func TestTokenWithoutExpiryRejected(t *testing.T) {
	claims := jwt.RegisteredClaims{Subject: uuid.NewString(), Issuer: issuer}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := NewTokens(testSecret, time.Hour).Parse(signed); err == nil {
		t.Fatal("token with no expiry accepted -- it would never stop working")
	}
}

func TestPasswordHashAndCheck(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("hash contains the plaintext")
	}
	if err := CheckPassword(hash, pw); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if err := CheckPassword(hash, pw+"!"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestSamePasswordHashesDifferently(t *testing.T) {
	// bcrypt salts per hash, so identical passwords must not produce identical
	// stored values -- otherwise a stolen table reveals which users share one.
	a, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("identical passwords produced identical hashes -- salting is broken")
	}
}

// bcrypt silently ignores input past 72 bytes, which would make two long
// passwords sharing a 72-byte prefix interchangeable. We reject instead.
func TestOverlongPasswordRejectedNotTruncated(t *testing.T) {
	long := strings.Repeat("a", 80)
	if _, err := HashPassword(long); err == nil {
		t.Fatal("an 80-byte password was accepted; bcrypt would silently truncate it")
	}

	// The byte limit must be measured in bytes, not runes: multi-byte
	// characters hit bcrypt's cap well before 72 characters.
	multibyte := strings.Repeat("é", 40) // 80 bytes, 40 runes
	if _, err := HashPassword(multibyte); err == nil {
		t.Fatal("an 80-byte multibyte password was accepted; the limit is counting runes")
	}
}

func TestShortPasswordRejected(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("a 5-character password was accepted")
	}
}

func TestMiddlewareRejectsMissingAndMalformedTokens(t *testing.T) {
	tk := NewTokens(testSecret, time.Hour)
	handler := Middleware(tk)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct{ name, header string }{
		{"absent", ""},
		{"no scheme", "abc.def.ghi"},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"empty bearer", "Bearer "},
		{"garbage", "Bearer not-a-jwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", rec.Code)
			}
			// RFC 9110 requires a challenge on a 401.
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate challenge")
			}
			// The failure reason must not distinguish "expired" from "bad
			// signature": that tells a forger which half worked.
			if body := rec.Body.String(); strings.Contains(strings.ToLower(body), "signature") {
				t.Errorf("response leaks the parse failure reason: %s", body)
			}
		})
	}
}

func TestMiddlewarePassesAuthenticatedUser(t *testing.T) {
	tk := NewTokens(testSecret, time.Hour)
	want := uuid.New()
	token, _, err := tk.Issue(want, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	var got uuid.UUID
	handler := Middleware(tk)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = httpx.MustUserID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer "+token) // scheme is case-insensitive
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got != want {
		t.Fatalf("user on context = %v, want %v", got, want)
	}
}
