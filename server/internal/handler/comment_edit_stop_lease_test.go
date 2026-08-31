package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestUpdateComment_EditedCommentWaitsForTheCancelledRunToStop covers the race
// MUL-6880 reports, end to end and in the order it actually happens.
//
// Editing a comment cancels the run it triggered and enqueues the replacement
// in the SAME request. Cancellation is instant server-side; the agent process
// is not — the daemon learns about it from its own poll tick and takes several
// seconds to tear the run down. Before this fix the replacement was claimable
// during that window, so it could not take the env root lock the dying run
// still held, started in a second workdir, and lost the provider session:
// the agent came back with no memory of the conversation it was editing.
//
// The replacement must therefore wait for the daemon's cancel-ack — posted
// after runTask returns, which is when the env root lock is released — and
// then resume the cancelled run's workdir and session.
func TestUpdateComment_EditedCommentWaitsForTheCancelledRunToStop(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID := createClaimReclaimRuntime(t, ctx, "Stop lease runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Stop lease agent")
	mention := "[@Stop lease agent](mention://agent/" + agentID + ")"

	const (
		runningWorkDir = "/tmp/multica-stop-lease/workdir"
		runningSession = "sess-stop-lease-1"
	)
	commentID := dbfx.Comment(t, issueID, mention+" first take")
	runningTaskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":         runtimeID,
		"issue_id":           issueID,
		"trigger_comment_id": commentID,
		"status":             "running",
		"dispatched_at":      testutil.Raw("now()"),
		"started_at":         testutil.Raw("now()"),
		"session_id":         runningSession,
		"work_dir":           runningWorkDir,
	})

	// The edit: cancels the run, enqueues the replacement.
	edit := newRequest(http.MethodPut, "/api/comments/"+commentID, map[string]any{
		"content": mention + " second take",
	})
	edit = withURLParam(edit, "commentId", commentID)
	testutil.Call(t, testHandler.UpdateComment, edit).Want(http.StatusOK)

	var status string
	var stopLeaseExpiresAt *time.Time
	dbfx.QueryRow(t, `
		SELECT status, stop_lease_expires_at FROM agent_task_queue WHERE id = $1
	`, runningTaskID).Scan(&status, &stopLeaseExpiresAt)
	if status != "cancelled" {
		t.Fatalf("edited comment left its run at %q, want cancelled", status)
	}
	if stopLeaseExpiresAt == nil || !stopLeaseExpiresAt.After(time.Now()) {
		t.Fatalf("cancelling a running task must arm a live stop lease, got %v", stopLeaseExpiresAt)
	}

	// Scan fails the test when the edit enqueued no replacement at all.
	var successorID string
	dbfx.QueryRow(t, `
		SELECT id FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
	`, issueID, agentID).Scan(&successorID)

	// While the lease is live the daemon must find nothing to claim, even
	// though a queued row for this (issue, agent) exists.
	req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil, testWorkspaceID, "stop-lease-daemon")
	req = withURLParam(req, "runtimeId", runtimeID)
	var held struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&held)
	if held.Task != nil {
		t.Fatalf("claimed %s while the cancelled run was still stopping; that is the MUL-6880 race", held.Task.ID)
	}

	// The daemon reports the run is really over.
	ack := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+runningTaskID+"/cancel-ack", nil, testWorkspaceID, "stop-lease-daemon")
	ack = withURLParam(ack, "taskId", runningTaskID)
	testutil.Call(t, testHandler.AckTaskCancelled, ack).Want(http.StatusOK)

	dbfx.QueryRow(t, `
		SELECT stop_lease_expires_at FROM agent_task_queue WHERE id = $1
	`, runningTaskID).Scan(&stopLeaseExpiresAt)
	if stopLeaseExpiresAt != nil {
		t.Fatalf("cancel-ack must release the stop lease, got %v", stopLeaseExpiresAt)
	}

	// Now the replacement runs — in the cancelled run's workdir, resuming its
	// session. That continuity is the whole point of making it wait.
	claim := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil, testWorkspaceID, "stop-lease-daemon")
	claim = withURLParam(claim, "runtimeId", runtimeID)
	var resumed struct {
		Task *struct {
			ID             string `json:"id"`
			PriorWorkDir   string `json:"prior_work_dir"`
			PriorSessionID string `json:"prior_session_id"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, claim).Want(http.StatusOK).JSON(&resumed)
	if resumed.Task == nil {
		t.Fatal("the replacement must be claimable once the daemon confirms the stop")
	}
	if resumed.Task.ID != successorID {
		t.Fatalf("claimed %s, want the replacement %s", resumed.Task.ID, successorID)
	}
	if resumed.Task.PriorWorkDir != runningWorkDir {
		t.Errorf("prior_work_dir = %q, want the cancelled run's %q", resumed.Task.PriorWorkDir, runningWorkDir)
	}
	if resumed.Task.PriorSessionID != runningSession {
		t.Errorf("prior_session_id = %q, want the cancelled run's %q", resumed.Task.PriorSessionID, runningSession)
	}
}
