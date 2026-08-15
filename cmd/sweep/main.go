// Command sweep deletes abandoned uploads.
//
//	go run ./cmd/sweep            # delete pending uploads older than 24h
//	go run ./cmd/sweep -age 1h    # a different cutoff
//	go run ./cmd/sweep -dry-run   # report only
//
// A command rather than a background scheduler, deliberately. A scheduler
// would add a job runner, a leader election among instances, and a whole class
// of "did it run" questions, to reclaim a small amount of storage. This runs
// from cron or by hand, is testable, and cannot silently stop.
//
// What it collects is precisely one thing: attachment rows still 'pending'
// long after their presigned URL expired, meaning the client never uploaded or
// never confirmed. An attachment that is ready but not mentioned in the note
// body is NOT garbage -- a file attached without being inlined is a normal
// case.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/config"
	"github.com/Trones21/bluestaq-takehome/internal/storage"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	age := flag.Duration("age", 24*time.Hour, "delete pending uploads older than this")
	batch := flag.Int("batch", 500, "maximum rows per run")
	dryRun := flag.Bool("dry-run", false, "report what would be deleted, delete nothing")
	flag.Parse()

	if err := run(*age, int32(*batch), *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "sweep: %v\n", err)
		os.Exit(1)
	}
}

func run(age time.Duration, batch int32, dryRun bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration invalid:\n%w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	objects, err := storage.New(cfg.Storage)
	if err != nil {
		return err
	}

	st := store.New(pool)
	cutoff := time.Now().Add(-age)

	stale, err := st.ListStaleUploads(ctx, sqlcgen.ListStaleUploadsParams{
		CreatedAt: cutoff,
		Limit:     batch,
	})
	if err != nil {
		return fmt.Errorf("listing stale uploads: %w", err)
	}
	if len(stale) == 0 {
		fmt.Printf("nothing to sweep (no pending uploads older than %s)\n", age)
		return nil
	}

	fmt.Printf("found %d abandoned upload(s) older than %s\n", len(stale), age)
	if dryRun {
		for _, a := range stale {
			fmt.Printf("  would delete %s (%s)\n", a.ID, a.ObjectKey)
		}
		return nil
	}

	var deleted, failed int
	for _, a := range stale {
		// Object first here, unlike the delete endpoint. A pending row has
		// never been visible to a caller, so the risk of a half-completed
		// delete is inverted: an object with no row is unreachable garbage,
		// while a row with no object would be retried harmlessly next run.
		if err := objects.Delete(ctx, a.ObjectKey); err != nil && !storage.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "  object %s: %v\n", a.ObjectKey, err)
			failed++
			continue
		}
		if _, err := st.DeleteAttachmentByID(ctx, a.ID); err != nil {
			fmt.Fprintf(os.Stderr, "  row %s: %v\n", a.ID, err)
			failed++
			continue
		}
		deleted++
	}

	fmt.Printf("swept %d, failed %d\n", deleted, failed)
	if failed > 0 {
		return fmt.Errorf("%d attachment(s) could not be swept", failed)
	}
	return nil
}
