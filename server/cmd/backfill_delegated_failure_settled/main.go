// backfill_delegated_failure_settled retires delegated-failure recovery
// comments that were already closed by a terminal delivery receipt before
// comment.recovery_settled_at existed.
//
// Run it once, after every backend in a rolling deployment carries the code
// that writes the marker. Until it finishes, the outbox scan behaves exactly as
// it did before — unmarked history is still scanned and no recovery is lost —
// so this is a performance recovery step, not a correctness step, and it is
// safe to run off-peak or in several sittings.
//
// Each batch commits independently and the walk resumes from an id keyset
// watermark, so SIGINT/SIGTERM or --max-batches can stop it at any point. The
// watermark advances over every candidate examined, not only the ones marked:
// a recovery that is genuinely still pending must be left alone and must not
// stall the walk. A session advisory lock keeps two operators from walking at
// once.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/delegatedrecoverybackfill"
	"github.com/multica-ai/multica/server/internal/logger"
)

const advisoryLockName = "delegated_failure_recovery_settled_backfill"

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("delegated failure recovery settled backfill failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	batchSize := flag.Int("batch-size", delegatedrecoverybackfill.DefaultBatchSize, "maximum candidate recovery comments examined per statement")
	delay := flag.Duration("sleep-between-batches", 100*time.Millisecond, "delay between committed batches")
	maxBatches := flag.Int("max-batches", 0, "stop after N batches (0 = walk the whole history)")
	flag.Parse()
	if *batchSize < 1 {
		return fmt.Errorf("--batch-size must be at least 1")
	}
	if *delay < 0 {
		return fmt.Errorf("--sleep-between-batches must not be negative")
	}
	if *maxBatches < 0 {
		return fmt.Errorf("--max-batches must not be negative")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire advisory-lock connection: %w", err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, advisoryLockName); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, advisoryLockName)
	}()

	remaining, err := delegatedrecoverybackfill.CountRemaining(ctx, pool, "")
	if err != nil {
		return err
	}
	slog.Info("delegated failure recovery settled backfill started",
		"unsettled", remaining, "batch_size", *batchSize, "delay", delay.String())

	var scanned, settled int64
	var afterID *string
	for batch := 1; *maxBatches == 0 || batch <= *maxBatches; batch++ {
		result, err := delegatedrecoverybackfill.Batch(ctx, pool, delegatedrecoverybackfill.Options{
			BatchSize: *batchSize,
			AfterID:   afterID,
		})
		if err != nil {
			return err
		}
		if result.Scanned == 0 {
			break
		}
		if result.LastID == "" {
			return fmt.Errorf("backfill batch examined %d candidates without a keyset watermark", result.Scanned)
		}
		scanned += result.Scanned
		settled += result.Settled
		slog.Info("delegated failure recovery settled batch committed",
			"batch", batch, "scanned", result.Scanned, "settled", result.Settled,
			"scanned_total", scanned, "settled_total", settled, "last_id", result.LastID)
		next := result.LastID
		afterID = &next
		if *delay > 0 {
			select {
			case <-time.After(*delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	remaining, err = delegatedrecoverybackfill.CountRemaining(ctx, pool, "")
	if err != nil {
		return err
	}
	// The remainder is the genuinely open backlog plus anything a reversible
	// exclusion is holding, which is what the index is supposed to contain.
	slog.Info("delegated failure recovery settled backfill finished",
		"scanned", scanned, "settled", settled, "unsettled_remaining", remaining)
	return nil
}
