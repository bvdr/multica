// Package delegatedrecoverybackfill provides bounded, resumable batches that
// retire delegated-failure recovery comments already closed by a terminal
// delivery receipt.
//
// migrations 444/445 add comment.recovery_settled_at and the partial index
// that only holds unsettled recovery comments, and TaskService writes the
// marker inside every terminal task transaction. Neither reaches the history
// written before the marker existed: those rows keep matching the index
// predicate and keep being re-proven settled by the sweeper's outbox scan on
// every tick. This walk marks them once.
//
// Run it after every backend in a rolling deployment carries the writing code.
// Running it earlier is not harmful — an in-flight task simply gets marked by
// whichever path reaches its terminal receipt first — but it leaves rows behind
// that a second pass has to pick up.
package delegatedrecoverybackfill

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultBatchSize bounds the candidate comments examined per statement. Each
// batch probes agent_task_queue for terminal receipts, so batches are kept
// large enough that the walk needs few of those probes; operators can add a
// delay between batches from the command wrapper.
const DefaultBatchSize = 1000

// Options controls one backfill batch. Zero values are production defaults.
type Options struct {
	BatchSize int
	// AfterID is the exclusive keyset watermark for the walk.
	AfterID *string
	// Schema is a trusted identifier override used only by tests.
	Schema string
}

// BatchResult advances a caller's keyset walk.
//
// The watermark tracks SCANNED candidates rather than settled ones: a recovery
// that is genuinely still pending — or that is currently excluded only by a
// reversible condition such as its issue sitting in 'done' — must never be
// marked, and must not stall the walk either. LastID is empty only when the
// batch found no candidates at all, which is how the caller detects the end.
type BatchResult struct {
	Scanned int64
	Settled int64
	LastID  string
}

func qualify(schema, table string) string {
	if schema == "" {
		return table
	}
	return fmt.Sprintf("%q.%s", schema, table)
}

// Batch settles at most BatchSize candidate recovery comments.
//
// A candidate is settled only when some terminal task already recorded it in
// delivered_comment_ids — the one exclusion in
// ListPendingDelegatedFailureRecoveries that cannot be taken back. Every other
// exclusion that query applies is reversible, so a row it currently skips for
// another reason is deliberately left pending here.
func Batch(ctx context.Context, pool *pgxpool.Pool, opts Options) (BatchResult, error) {
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	comment := qualify(opts.Schema, "comment")
	task := qualify(opts.Schema, "agent_task_queue")

	query := fmt.Sprintf(`
WITH batch AS (
    SELECT id
    FROM %s
    WHERE author_type = 'system'
      AND type = 'progress_update'
      AND source_task_id IS NOT NULL
      AND recovery_settled_at IS NULL
      AND ($2::uuid IS NULL OR id > $2::uuid)
    ORDER BY id
    LIMIT $1
), settled AS (
    UPDATE %s recovery
    SET recovery_settled_at = now()
    WHERE recovery.id IN (SELECT id FROM batch)
      AND recovery.recovery_settled_at IS NULL
      AND EXISTS (
          SELECT 1
          FROM %s covering
          WHERE covering.status IN ('completed', 'failed', 'cancelled')
            AND covering.delivered_comment_ids @> ARRAY[recovery.id]
      )
    RETURNING recovery.id
)
SELECT (SELECT count(*)::bigint FROM batch),
       (SELECT count(*)::bigint FROM settled),
       COALESCE((SELECT id::text FROM batch ORDER BY id DESC LIMIT 1), '')`, comment, comment, task)

	var result BatchResult
	if err := pool.QueryRow(ctx, query, batchSize, opts.AfterID).
		Scan(&result.Scanned, &result.Settled, &result.LastID); err != nil {
		return BatchResult{}, fmt.Errorf("backfill delegated failure recovery settled batch: %w", err)
	}
	return result, nil
}

// CountRemaining reports how many recovery comments are still unsettled, which
// is exactly the row count of idx_comment_delegated_failure_unsettled. After a
// completed walk this should be the genuinely pending backlog, not history.
func CountRemaining(ctx context.Context, pool *pgxpool.Pool, schema string) (int64, error) {
	var count int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
SELECT count(*)
FROM %s
WHERE author_type = 'system'
  AND type = 'progress_update'
  AND source_task_id IS NOT NULL
  AND recovery_settled_at IS NULL`, qualify(schema, "comment"))).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unsettled delegated failure recoveries: %w", err)
	}
	return count, nil
}
