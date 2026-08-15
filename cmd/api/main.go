// Command api runs the notes service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/auth"
	"github.com/Trones21/bluestaq-takehome/internal/config"
	"github.com/Trones21/bluestaq-takehome/internal/notes"
	"github.com/Trones21/bluestaq-takehome/internal/obs"
	"github.com/Trones21/bluestaq-takehome/internal/server"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/users"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if config failed, so this one goes to
		// stderr directly.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration invalid:\n%w", err)
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	// Signal context first: a SIGTERM arriving during startup should abort
	// startup rather than be ignored until the server is listening.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := newPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	st := store.New(pool)
	tokens := auth.NewTokens(cfg.JWTSecret, cfg.JWTTTL)
	metrics := obs.NewMetrics()

	deps := server.Deps{
		Config:  cfg,
		Logger:  log,
		Metrics: metrics,
		DB:      pool,
		Tokens:  tokens,
		Users:   users.New(st, tokens),
		Notes:   notes.New(st, metrics),
	}

	public := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: server.PublicRouter(deps),
		// Timeouts are not optional: without them a slow or malicious client
		// holds a connection and a goroutine indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	admin := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.AdminPort),
		Handler:           server.AdminRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	serve := func(name string, s *http.Server) {
		log.Info("listening", slog.String("server", name), slog.String("addr", s.Addr))
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s server: %w", name, err)
		}
	}
	go serve("public", public)
	go serve("admin", admin)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
	}

	// Graceful shutdown: stop accepting, let in-flight requests finish. Without
	// this every deploy returns a connection error to whatever was mid-request.
	// The bounded context means a stuck handler cannot block the deploy forever.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer cancel()

	// Public first so we stop taking new work before the admin listener goes
	// away -- readiness should keep answering while we drain.
	if err := public.Shutdown(shutdownCtx); err != nil {
		log.Error("public server did not drain cleanly", slog.Any("err", err))
	}
	if err := admin.Shutdown(shutdownCtx); err != nil {
		log.Error("admin server did not drain cleanly", slog.Any("err", err))
	}

	log.Info("stopped")
	return nil
}

func newPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}
	// Bounded on purpose: an unbounded pool turns a traffic spike into
	// "too many connections" on a database shared with everything else.
	poolCfg.MaxConns = cfg.MaxDBConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}

	// Fail fast at boot rather than on the first request.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
