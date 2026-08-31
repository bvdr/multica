-- MUL-6880: hold the per-(issue, agent) execution slot until the daemon
-- confirms a cancelled run actually stopped.
--
-- ClaimAgentTask already serializes per (issue, agent): a task is claimable
-- only while no sibling row is dispatched/running/waiting_local_directory.
-- That fence reads DB status, and cancellation flips status to 'cancelled'
-- IMMEDIATELY while the daemon's agent process keeps running for another
-- 4-10s (it learns of the cancellation from its own poll/reconcile tick).
-- The window is wide open on the most ordinary flow there is: editing a
-- comment cancels its run and enqueues the replacement in the same request.
-- The replacement is then claimed while the predecessor still holds the env
-- root lock, so it cannot reuse the workdir, and reusing the workdir is what
-- carries the provider session — the agent restarts with no memory of the
-- conversation it was in the middle of.
--
-- stop_lease_expires_at keeps the slot occupied across that window: armed
-- when a run that was actually executing is cancelled, released by the
-- daemon's cancel-ack (posted after runTask returns, i.e. after the env root
-- lock is released), and bounded by the deadline so a daemon that dies
-- without acking cannot wedge the successor.
ALTER TABLE agent_task_queue
    ADD COLUMN stop_lease_expires_at TIMESTAMPTZ;

COMMENT ON COLUMN agent_task_queue.stop_lease_expires_at IS
    'Cancelled-but-maybe-still-executing marker. While in the future, this row keeps blocking ClaimAgentTask for its (issue, agent) so a successor cannot race the dying run for the env root / provider session. Armed by trg_arm_task_stop_lease, cleared by the daemon cancel-ack and by runtime orphan recovery (MUL-6880).';

-- 30s: the daemon notices a server-side cancellation within one cancel-poll
-- interval (5s) and acks a couple of seconds after the agent process exits,
-- so the ack normally releases the slot long before this. The deadline only
-- has to cover a daemon that never acks at all — a lost POST, a crash
-- between kill and ack — and a successor that waits half a minute in that
-- case is strictly better than one that starts against a live lock.
CREATE OR REPLACE FUNCTION arm_task_stop_lease_on_cancel()
RETURNS TRIGGER AS $$
BEGIN
    -- Only a row that was EXECUTING has a process to wait for. A queued or
    -- deferred row was never dispatched to a daemon, so cancelling it must
    -- not delay anything.
    IF NEW.status = 'cancelled'
       AND OLD.status IN ('dispatched', 'running', 'waiting_local_directory') THEN
        NEW.stop_lease_expires_at := now() + interval '30 seconds';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Trigger-based rather than per-query SET clauses for the same reason as
-- trg_clear_runtime_mcp_overlay (migration 128): the cancel transitions live
-- in a dozen sqlc queries (CancelAgentTask, CancelAgentTaskByUser,
-- CancelAgentTaskWithReason, CancelAgentTasksByIssue / ByAgent /
-- ByTriggerComment / ByChatSession, ...), and the invariant "a server-side
-- cancel of a live run holds the slot until the daemon confirms it stopped"
-- must not be something a future cancel query can forget to opt into.
DROP TRIGGER IF EXISTS trg_arm_task_stop_lease ON agent_task_queue;
CREATE TRIGGER trg_arm_task_stop_lease
    BEFORE UPDATE OF status ON agent_task_queue
    FOR EACH ROW
    EXECUTE FUNCTION arm_task_stop_lease_on_cancel();
