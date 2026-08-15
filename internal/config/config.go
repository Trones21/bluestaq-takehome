// Package config loads and validates process configuration from the environment.
//
// Everything is parsed and validated once, at startup, and the process exits on
// anything missing or malformed. A config error should surface when the container
// starts, not on the first request that happens to need the value.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Public listener: the API.
	Port int
	// Admin listener: /metrics, /healthz, /readyz. Deliberately separate --
	// metrics leak route structure, request volumes, and error rates.
	AdminPort int

	DatabaseURL string
	MaxDBConns  int32

	// RequestTimeout bounds how long a single request may spend in handler
	// code. It is the deadline on the request context, which is what actually
	// unblocks a goroutine parked waiting for a pool connection: MaxDBConns
	// bounds concurrent queries, but without a deadline the queue of waiters
	// behind it is unbounded. See DISCUSSION 13.
	RequestTimeout time.Duration

	// StatementTimeout and IdleInTxTimeout are set on every pooled connection
	// and enforced by Postgres, not by this process. That distinction is the
	// point: cancelling a context breaks the client socket, but Postgres does
	// not check whether the client is still there mid-statement
	// (client_connection_check_interval defaults to 0), so the backend keeps
	// working until it tries to write results. These are the only bounds that
	// stop the work rather than abandon it.
	//
	// They cover different failure modes. StatementTimeout bounds one running
	// statement; IdleInTxTimeout bounds a transaction that is open but
	// executing nothing -- the client stalled between statements, holding
	// locks and a pool connection the whole time. A statement timeout cannot
	// catch that, because no statement is running.
	StatementTimeout time.Duration
	IdleInTxTimeout  time.Duration

	JWTSecret    []byte
	JWTTTL       time.Duration
	LogLevel     string
	CORSOrigins  []string
	ShutdownWait time.Duration

	Storage StorageConfig
}

type StorageConfig struct {
	Endpoint     string // empty => real AWS S3
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool // MinIO needs this; real S3 does not

	// Upload URLs are short-lived: the client uploads immediately.
	// Download URLs live longer so images do not break under a reader who
	// keeps a note open. See SPEC 7.
	UploadTTL   time.Duration
	DownloadTTL time.Duration

	MaxAttachmentBytes int64
	AllowedMIMEPrefix  []string
}

// MaxRequestBytes caps the JSON request body. Attachment bytes never pass
// through the API, so this stays small on purpose.
const MaxRequestBytes = 1 << 20 // 1 MiB

// Socket-level deadlines for the public listener. These guard the connection;
// RequestTimeout guards the handler. They are different jobs: WriteTimeout
// kills the socket but does not cancel the request context, so it cannot free
// a goroutine blocked on the database.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 30 * time.Second
	WriteTimeout      = 60 * time.Second
	IdleTimeout       = 120 * time.Second
)

// Load reads configuration from the environment, collecting every problem
// rather than failing on the first one -- a misconfigured deploy should
// report all of its errors in one pass instead of one per restart.
func Load() (*Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	cfg := &Config{
		Port:        envInt("PORT", 8080, fail),
		AdminPort:   envInt("ADMIN_PORT", 9090, fail),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		MaxDBConns:  int32(envInt("DB_MAX_CONNS", 10, fail)),
		// Well inside the 60s WriteTimeout: the deadline is only useful if it
		// fires while the server is still willing to write the response.
		RequestTimeout: envDuration("REQUEST_TIMEOUT", 15*time.Second, fail),
		// Every transaction here is a handful of statements against indexed
		// rows -- no object storage call, no external I/O inside a
		// transaction. Real work finishes in milliseconds, so these are blast
		// radius limits rather than performance tuning: hitting either one is
		// a bug or a pathological plan, not a slow but legitimate request.
		StatementTimeout: envDuration("STATEMENT_TIMEOUT", 3*time.Second, fail),
		IdleInTxTimeout:  envDuration("IDLE_IN_TX_TIMEOUT", 10*time.Second, fail),
		JWTSecret:        []byte(os.Getenv("JWT_SECRET")),
		JWTTTL:           envDuration("JWT_TTL", 24*time.Hour, fail),
		LogLevel:         envDefault("LOG_LEVEL", "info"),
		CORSOrigins:      envList("CORS_ALLOWED_ORIGINS"),
		ShutdownWait:     envDuration("SHUTDOWN_WAIT", 15*time.Second, fail),
		Storage: StorageConfig{
			Endpoint:           os.Getenv("S3_ENDPOINT"),
			Region:             envDefault("S3_REGION", "us-east-1"),
			Bucket:             os.Getenv("S3_BUCKET"),
			AccessKey:          os.Getenv("S3_ACCESS_KEY"),
			SecretKey:          os.Getenv("S3_SECRET_KEY"),
			UsePathStyle:       envBool("S3_PATH_STYLE", true, fail),
			UploadTTL:          envDuration("S3_UPLOAD_TTL", 15*time.Minute, fail),
			DownloadTTL:        envDuration("S3_DOWNLOAD_TTL", time.Hour, fail),
			MaxAttachmentBytes: int64(envInt("MAX_ATTACHMENT_MB", 100, fail)) << 20,
			AllowedMIMEPrefix: envListDefault("ALLOWED_MIME",
				[]string{"image/", "video/mp4", "application/pdf"}),
		},
	}

	if cfg.DatabaseURL == "" {
		fail("DATABASE_URL is required")
	}
	// A short or absent signing key is an auth bypass waiting to happen, so it
	// is a startup error rather than a warning.
	if len(cfg.JWTSecret) < 32 {
		fail("JWT_SECRET is required and must be at least 32 bytes (got %d)", len(cfg.JWTSecret))
	}
	if cfg.Storage.Bucket == "" {
		fail("S3_BUCKET is required")
	}
	if cfg.Port == cfg.AdminPort {
		fail("PORT and ADMIN_PORT must differ: the admin listener must not be publicly routable")
	}
	if cfg.Storage.DownloadTTL > 24*time.Hour {
		fail("S3_DOWNLOAD_TTL above 24h widens the revoke gap unacceptably")
	}
	if cfg.RequestTimeout <= 0 {
		fail("REQUEST_TIMEOUT must be positive, got %s", cfg.RequestTimeout)
	}
	// Past WriteTimeout the server has already abandoned the socket, so the
	// deadline would fire too late to turn into a response the client sees.
	if cfg.RequestTimeout >= WriteTimeout {
		fail("REQUEST_TIMEOUT (%s) must be below the %s write timeout", cfg.RequestTimeout, WriteTimeout)
	}
	if cfg.StatementTimeout <= 0 {
		fail("STATEMENT_TIMEOUT must be positive, got %s", cfg.StatementTimeout)
	}
	if cfg.IdleInTxTimeout <= 0 {
		fail("IDLE_IN_TX_TIMEOUT must be positive, got %s", cfg.IdleInTxTimeout)
	}
	// The budgets have to nest. If a statement may outlive the request that
	// issued it, the outer deadline always fires first and the specific error
	// -- the one that says which limit was hit -- is never the one returned.
	if cfg.StatementTimeout >= cfg.RequestTimeout {
		fail("STATEMENT_TIMEOUT (%s) must be below REQUEST_TIMEOUT (%s), or the request deadline always wins",
			cfg.StatementTimeout, cfg.RequestTimeout)
	}

	return cfg, errors.Join(errs...)
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, fail func(string, ...any)) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fail("%s must be an integer, got %q", key, v)
		return def
	}
	return n
}

func envBool(key string, def bool, fail func(string, ...any)) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fail("%s must be a boolean, got %q", key, v)
		return def
	}
	return b
}

func envDuration(key string, def time.Duration, fail func(string, ...any)) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fail("%s must be a duration such as 15m or 24h, got %q", key, v)
		return def
	}
	return d
}

func envList(key string) []string {
	return envListDefault(key, nil)
}

func envListDefault(key string, def []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
