package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// The bind-vs-revoke race, both orderings, on two real connections (MUL-6704).
//
// The invariant: a private runtime never ends up with a foreign agent bound and no
// teardown. The lock modes make it subtle — an agent write validates its runtime FK
// under FOR KEY SHARE while a plain non-key UPDATE takes FOR NO KEY UPDATE, and
// those do not conflict, so a bind and a "nothing to tear down" flip could both
// commit. Each side now takes a lock that does conflict (LockAgentRuntime FOR
// UPDATE on the revoke; LockAgentRuntimeForBind FOR KEY SHARE plus a re-read on the
// binder). Each test holds one side open and asserts the other blocks and then sees
// the committed state, not its starting snapshot.

// waitBlocked is a lower bound on "definitely blocked", not a timeout: the
// assertions that matter run after the holding transaction commits.
const waitBlocked = 250 * time.Millisecond

type handlerResult struct {
	code int
	body string
}

// A bind is in flight, holding FOR KEY SHARE, when the owner makes the machine
// private with (as far as an unlocked read can tell) nothing bound. The PATCH must
// block, then see that agent and refuse. Pre-fix it returned 200 and flipped.
func TestVisibilityRevokeRace_BindHoldsLockFirst(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, agentID := raceFixture(t, ctx, "Race Bind First")

	// Connection 1: the binder — same FK-shaped lock a real bind takes, held open.
	commit := holdTx(t, ctx, func(tx pgx.Tx) {
		mustExec(t, ctx, tx, `SELECT 1 FROM agent_runtime WHERE id = $1 FOR KEY SHARE`, runtimeID)
		mustExec(t, ctx, tx, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, agentID)
	})

	// Connection 2: the PATCH, on its own pooled connection.
	done := make(chan handlerResult, 1)
	go func() {
		w := patchVisibilityPrivate(t, runtimeID)
		done <- handlerResult{code: w.Code, body: w.Body.String()}
	}()
	requireBlocked(t, done, "PATCH", "it must wait for FOR UPDATE")
	commit()

	res := waitForHandler(t, done)
	if res.code != http.StatusConflict {
		t.Fatalf("PATCH after the bind committed: got %d, want 409 with the impact plan.\nbody: %s", res.code, res.body)
	}
	var body revokePlanBody
	if err := json.Unmarshal([]byte(res.body), &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if body.Code != runtimeVisibilityHasForeignAgentsCode {
		t.Fatalf("code = %q, want %q", body.Code, runtimeVisibilityHasForeignAgentsCode)
	}
	if len(body.ActiveAgents) != 1 || body.ActiveAgents[0].ID != agentID {
		t.Fatalf("the recount must include the agent that landed during the wait, got %+v", body.ActiveAgents)
	}
	if got := runtimeVisibility(t, runtimeID); got != "public" {
		t.Fatalf("visibility = %q; a runtime with a foreign agent must not have been flipped", got)
	}
}

// Mirror image: the revoke holds FOR UPDATE and has written `private` when a bind
// arrives. The bind must block and then be refused, not proceed on the `public`
// snapshot it read before the wait — pre-fix it blocked and then wrote anyway.
func TestVisibilityRevokeRace_RevokeHoldsLockFirst(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, agentID := raceFixture(t, ctx, "Race Revoke First")
	var startingRuntime string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&startingRuntime)

	// Connection 1: the revoke mid-transaction — locked FOR UPDATE, already flipped.
	commit := holdTx(t, ctx, func(tx pgx.Tx) {
		mustExec(t, ctx, tx, `SELECT 1 FROM agent_runtime WHERE id = $1 FOR UPDATE`, runtimeID)
		mustExec(t, ctx, tx, `UPDATE agent_runtime SET visibility = 'private' WHERE id = $1`, runtimeID)
	})

	// Connection 2: a rebind of the foreign agent, through the real handler.
	done := make(chan handlerResult, 1)
	go func() {
		w := httptest.NewRecorder()
		req := newRequest("PATCH", "/api/agents/"+agentID, map[string]any{"runtime_id": runtimeID})
		testHandler.UpdateAgent(w, withURLParam(req, "id", agentID))
		done <- handlerResult{code: w.Code, body: w.Body.String()}
	}()
	// A 403 before the commit would mean the pre-flight check caught it, not the
	// lock: the runtime was still public when this request read it.
	requireBlocked(t, done, "rebind", "it must wait")
	commit()

	res := waitForHandler(t, done)
	if res.code != http.StatusForbidden {
		t.Fatalf("rebind after the revoke committed: got %d, want 403 — it must re-read the row, not trust its pre-wait snapshot.\nbody: %s",
			res.code, res.body)
	}
	var boundRuntime string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&boundRuntime)
	if boundRuntime != startingRuntime {
		t.Fatalf("agent runtime_id = %s; the refused bind must not have moved it onto the reclaimed machine", boundRuntime)
	}
}

// raceFixture returns a public runtime to reclaim and a teammate's agent that
// currently lives on a DIFFERENT public runtime — the bind under test is what
// moves it onto the machine being reclaimed.
func raceFixture(t *testing.T, ctx context.Context, name string) (runtimeID, agentID string) {
	t.Helper()
	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, name+" Runtime")
	otherRuntimeID := dbfx.Runtime(t, name+" Other", testutil.Cols{"visibility": "public"})
	return runtimeID, dbfx.Agent(t, name+" Agent", otherRuntimeID, ownedBy(foreignUserID))
}

// holdTx runs seed inside a transaction and returns a commit function, so a test
// can keep the transaction's locks held while it drives a second connection.
func holdTx(t *testing.T, ctx context.Context, seed func(pgx.Tx)) func() {
	t.Helper()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holding tx: %v", err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			tx.Rollback(context.Background())
		}
	})
	seed(tx)
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit holding tx: %v", err)
		}
		committed = true
	}
}

func mustExec(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("holding tx exec %q: %v", sql, err)
	}
}

func requireBlocked(t *testing.T, done <-chan handlerResult, what, why string) {
	t.Helper()
	select {
	case res := <-done:
		t.Fatalf("%s completed while the other side held the runtime lock (code %d): %s.\nbody: %s",
			what, res.code, why, res.body)
	case <-time.After(waitBlocked):
	}
}

func waitForHandler(t *testing.T, done <-chan handlerResult) handlerResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("blocked request never completed after the holding transaction committed")
		return handlerResult{}
	}
}
