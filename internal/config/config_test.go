package config

import (
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/notes",
		"JWT_SECRET":   strings.Repeat("k", 32),
		"S3_BUCKET":    "notes-attachments",
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	setEnv(t, validEnv())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.Port == cfg.AdminPort {
		t.Fatal("default ports collide")
	}
	// Downloads outlive uploads on purpose: a reader with a note open for an
	// hour should not watch its images break, while an upload happens at once.
	if cfg.Storage.DownloadTTL <= cfg.Storage.UploadTTL {
		t.Fatalf("download TTL (%v) must exceed upload TTL (%v)",
			cfg.Storage.DownloadTTL, cfg.Storage.UploadTTL)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	// A misconfigured deploy should surface all of its errors in one pass
	// rather than one per restart.
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", "")
	t.Setenv("S3_BUCKET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("empty environment accepted")
	}
	for _, want := range []string{"DATABASE_URL", "JWT_SECRET", "S3_BUCKET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestShortJWTSecretIsRejected(t *testing.T) {
	env := validEnv()
	env["JWT_SECRET"] = "short"
	setEnv(t, env)

	if _, err := Load(); err == nil {
		t.Fatal("a 5-byte signing key was accepted; that is a token-forgery risk")
	}
}

func TestAdminPortMustDifferFromPublicPort(t *testing.T) {
	env := validEnv()
	env["PORT"] = "8080"
	env["ADMIN_PORT"] = "8080"
	setEnv(t, env)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ADMIN_PORT") {
		t.Fatalf("sharing a port publishes /metrics; want a startup error, got %v", err)
	}
}

func TestOverlongDownloadTTLRejected(t *testing.T) {
	env := validEnv()
	env["S3_DOWNLOAD_TTL"] = "72h"
	setEnv(t, env)

	if _, err := Load(); err == nil {
		t.Fatal("a 72h presigned URL is effectively unrevokable; want a startup error")
	}
}

func TestMalformedValuesAreStartupErrors(t *testing.T) {
	env := validEnv()
	env["PORT"] = "eighty-eighty"
	env["S3_UPLOAD_TTL"] = "fifteen minutes"
	setEnv(t, env)

	_, err := Load()
	if err == nil {
		t.Fatal("malformed values accepted")
	}
	for _, want := range []string{"PORT", "S3_UPLOAD_TTL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}
