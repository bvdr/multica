package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestStopLeaseHoldsTheExecutionSlotUntilTheDaemonConfirms covers the SQL half
// of MUL-6880: which cancellations arm a stop lease, what a live lease blocks,
// and every way it is released.
//
// The invariant: a run that was EXECUTING when the server cancelled it keeps
// owning its (issue, agent) slot until the daemon proves the process is gone.
// Until then the workdir is still locked and the provider session still
// belongs to the dying run, so a successor that claims early cannot have
// either.
func TestStopLeaseHoldsTheExecutionSlotUntilTheDaemonConfirms(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
	})

	seedTask := func(status, issue string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, dispatched_at, started_at)
			VALUES ($1, $2, $3, $4, 0,
				CASE WHEN $4 = 'queued' THEN NULL ELSE now() END,
				CASE WHEN $4 IN ('running', 'waiting_local_directory') THEN now() ELSE NULL END)
			RETURNING id
		`, agentID, runtimeID, issue, status).Scan(&id); err != nil {
			t.Fatalf("insert %s task: %v", status, err)
		}
		return id
	}
	leaseOf := func(taskID string) *string {
		t.Helper()
		var lease *string
		if err := pool.QueryRow(ctx, `SELECT stop_lease_expires_at::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&lease); err != nil {
			t.Fatalf("read stop lease: %v", err)
		}
		return lease
	}
	claim := func() (db.AgentTaskQueue, error) {
		return q.ClaimAgentTask(ctx, db.ClaimAgentTaskParams{
			AgentID:          util.MustParseUUID(agentID),
			RuntimeID:        util.MustParseUUID(runtimeID),
			PrepareLeaseSecs: 60,
			RuntimeStaleSecs: RuntimeClaimFreshnessSeconds,
		})
	}

	t.Run("only a run that was executing arms a lease", func(t *testing.T) {
		for _, status := range []string{"running", "dispatched", "waiting_local_directory"} {
			taskID := seedTask(status, issueID)
			if _, err := q.CancelAgentTask(ctx, util.MustParseUUID(taskID)); err != nil {
				t.Fatalf("cancel %s task: %v", status, err)
			}
			if leaseOf(taskID) == nil {
				t.Errorf("cancelling a %s task armed no stop lease; its process is still running", status)
			}
			pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		}

		// A queued row was never handed to a daemon, so there is no process to
		// wait for and delaying its successor would be pure latency.
		taskID := seedTask("queued", issueID)
		if _, err := q.CancelAgentTask(ctx, util.MustParseUUID(taskID)); err != nil {
			t.Fatalf("cancel queued task: %v", err)
		}
		if lease := leaseOf(taskID); lease != nil {
			t.Errorf("cancelling a queued task armed a stop lease (%v); nothing was executing", *lease)
		}
		pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})

	t.Run("a live lease blocks the successor and every release frees it", func(t *testing.T) {
		cancelledID := seedTask("running", issueID)
		if _, err := q.CancelAgentTask(ctx, util.MustParseUUID(cancelledID)); err != nil {
			t.Fatalf("cancel running task: %v", err)
		}
		successorID := seedTask("queued", issueID)
		t.Cleanup(func() {
			pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = ANY($1::uuid[])`, []string{cancelledID, successorID})
		})

		if _, err := claim(); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("claim during the stop window = %v, want no rows", err)
		}

		// A task on another issue shares nothing with the dying run — same
		// agent, different workdir — so it must not be held back.
		otherIssueID := seedIssue(t, pool, workspaceID, userID, agentID, "stop lease other issue")
		otherID := seedTask("queued", otherIssueID)
		t.Cleanup(func() {
			pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, otherID)
		})
		claimed, err := claim()
		if err != nil {
			t.Fatalf("claim on an unaffected issue: %v", err)
		}
		if util.UUIDToString(claimed.ID) != otherID {
			t.Fatalf("claimed %s, want the unaffected issue's task %s", util.UUIDToString(claimed.ID), otherID)
		}
		pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, otherID)

		// The daemon's cancel-ack releases it, and the successor goes now.
		released, err := q.ReleaseAgentTaskStopLease(ctx, util.MustParseUUID(cancelledID))
		if err != nil {
			t.Fatalf("release stop lease: %v", err)
		}
		if released.StopLeaseExpiresAt.Valid {
			t.Errorf("released row still carries a lease: %v", released.StopLeaseExpiresAt.Time)
		}
		claimed, err = claim()
		if err != nil {
			t.Fatalf("claim after cancel-ack: %v", err)
		}
		if util.UUIDToString(claimed.ID) != successorID {
			t.Fatalf("claimed %s, want the successor %s", util.UUIDToString(claimed.ID), successorID)
		}

		// A replayed ack releases nothing, so the caller can skip its wakeup.
		if _, err := q.ReleaseAgentTaskStopLease(ctx, util.MustParseUUID(cancelledID)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("replayed release = %v, want no rows", err)
		}
	})

	t.Run("an expired lease stops blocking", func(t *testing.T) {
		cancelledID := seedTask("running", issueID)
		if _, err := q.CancelAgentTask(ctx, util.MustParseUUID(cancelledID)); err != nil {
			t.Fatalf("cancel running task: %v", err)
		}
		successorID := seedTask("queued", issueID)
		t.Cleanup(func() {
			pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = ANY($1::uuid[])`, []string{cancelledID, successorID})
		})

		// A daemon that dies between killing the agent and posting its ack
		// never releases the lease. The deadline is what keeps that from
		// wedging the issue instead of costing it a few seconds.
		if _, err := pool.Exec(ctx, `
			UPDATE agent_task_queue SET stop_lease_expires_at = now() - interval '1 second' WHERE id = $1
		`, cancelledID); err != nil {
			t.Fatalf("expire stop lease: %v", err)
		}
		claimed, err := claim()
		if err != nil {
			t.Fatalf("claim after the lease expired: %v", err)
		}
		if util.UUIDToString(claimed.ID) != successorID {
			t.Fatalf("claimed %s, want the successor %s", util.UUIDToString(claimed.ID), successorID)
		}
	})

	t.Run("runtime recovery drops the leases a restarted daemon left behind", func(t *testing.T) {
		cancelledID := seedTask("running", issueID)
		if _, err := q.CancelAgentTask(ctx, util.MustParseUUID(cancelledID)); err != nil {
			t.Fatalf("cancel running task: %v", err)
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, cancelledID)
		})

		released, err := q.ReleaseAgentTaskStopLeasesForRuntime(ctx, util.MustParseUUID(runtimeID))
		if err != nil {
			t.Fatalf("release runtime stop leases: %v", err)
		}
		if len(released) != 1 || util.UUIDToString(released[0].ID) != cancelledID {
			t.Fatalf("released %d leases, want just %s", len(released), cancelledID)
		}
		if lease := leaseOf(cancelledID); lease != nil {
			t.Errorf("lease survived runtime recovery: %v", *lease)
		}
	})
}

func seedIssue(t *testing.T, pool *pgxpool.Pool, workspaceID, userID, agentID, title string) string {
	t.Helper()
	var issueID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id, assignee_type, assignee_id, priority, number)
		VALUES ($1, $2, 'member', $3, 'agent', $4, 'medium',
			(SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
		RETURNING id
	`, workspaceID, title, userID, agentID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })
	return issueID
}
