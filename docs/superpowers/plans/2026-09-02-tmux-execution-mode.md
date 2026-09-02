# tmux Execution Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a local-directory project resource (or a workspace default folder) run tasks as an interactive Claude Code session inside tmux on the runtime machine, tracked to completion by the daemon.

**Architecture:** A third `execution_mode` value, `tmux`, rides on the existing local-directory resource and is gated by a new daemon capability exactly like `worktree`. The daemon prepares the folder as it does for `in_place`, then spawns `tmux new-session` running a generated `run.sh` that launches interactive `claude` with the task prompt, records the exit code, and tees the pane to a transcript; a watch loop turns the session's end into the normal complete/fail report. A nullable JSONB column on `workspace` holds a default folder that the claim handler synthesizes into a project resource when the project has none.

**Tech Stack:** Go 1.26 (server + daemon, tests run in the `golang:1.26-alpine` container against a throwaway pgvector Postgres), sqlc 1.31.1, PostgreSQL migrations, TypeScript/React with TanStack Query and zod (Vitest), tmux 3.x, Claude Code CLI.

**Spec:** `docs/superpowers/specs/2026-09-02-tmux-execution-mode-design.md`

## Global Constraints

- Capability string is exactly `local-tmux-v1`; execution mode value is exactly `tmux`; workspace column is exactly `default_local_directory` (JSONB, nullable); migration number is `444`.
- tmux session names are `ctx-<issue identifier>-<first 4 chars of task id>`, lower-cased, any character outside `[a-z0-9-]` replaced by `-`, with `-2`, `-3`, … appended on collision.
- The interactive launch never passes `-p`, `--print`, `--output-format`, `--input-format`, `--permission-mode`, `--dangerously-skip-permissions`, `--resume`, or `--disallowedTools`.
- Transcript tail is the last 200 lines with ANSI escapes stripped; the watch loop polls every 2 seconds.
- tmux tasks skip the per-path mutex; the user's folder is never deleted or GC'd; task state lives under `<WorkspacesRoot>/.tmux-tasks/<taskID>/` (dot-prefixed, so the GC skips it).
- No foreign keys, no cascades, no non-concurrent index in migrations (this change adds no index).
- Default tests never resolve real `tmux` or `claude`; they install fake executables in a temp dir first on `PATH`.
- Copy says ContextPRO. Package names, env vars, cookie keys, URLs and the `multica` CLI command keep their names.
- No Go toolchain on the Mac Mini: run every Go command through the container recipe in "How to run Go checks" below. Frontend tests need `NODE_OPTIONS=--no-experimental-webstorage`.
- Commit after each task with a conventional prefix. Never commit `.env`.

## How to run Go checks

```bash
cd /Users/bogdand/Gits/bvdr/multica
docker network create multica-test-go >/dev/null 2>&1 || true
docker rm -f multica-test-go-pg >/dev/null 2>&1 || true
docker run -d --name multica-test-go-pg --network multica-test-go \
  -e POSTGRES_USER=multica -e POSTGRES_PASSWORD=multica -e POSTGRES_DB=multica pgvector/pgvector:pg17 >/dev/null && sleep 4
# GOTEST is reused by every task below. Add packages after "go test".
GOTEST='docker run --rm --network multica-test-go -e DATABASE_URL=postgres://multica:multica@multica-test-go-pg:5432/multica?sslmode=disable -e APP_ENV= -e JWT_SECRET=test-only-secret -v "$PWD":/repo -w /repo/server -v multica-gomod:/go/pkg/mod -v multica-gocache:/root/.cache/go-build golang:1.26-alpine sh -c'
$GOTEST 'go run ./cmd/migrate up >/dev/null; echo migrated'
# Example: $GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run TestTmux -count=1 -v'
# Daemon tests that spawn processes must run as a non-root user (Claude Code refuses root):
#   prefix the sh -c body with: apk add --no-cache git bash >/dev/null; adduser -D tester; chown -R tester /go/pkg/mod; mkdir -p /tmp/gocache && chown tester /tmp/gocache; su tester -c "cd /repo/server && HOME=/tmp GOCACHE=/tmp/gocache <go test ...>"
# Teardown at the very end: docker rm -f multica-test-go-pg; docker network rm multica-test-go
```

## File Structure

**Server (Go)**
- Modify `server/pkg/protocol/messages.go` — add `DaemonCapabilityLocalTmuxV1`.
- Modify `server/internal/handler/project_resource.go` — `localDirectoryModeTmux`, `validateLocalDirectoryRef`, mode→capability table, generalized save-time gate `requireModeCapableDaemon` (replaces `requireWorktreeCapableDaemon` at its two call sites).
- Modify `server/internal/handler/daemon.go` — `tmuxClaimBlockReason`, called next to `worktreeClaimBlockReason`; claim gate reads both.
- Modify `server/internal/handler/config.go` — `LocalTmuxSupported`.
- Create `server/migrations/444_workspace_default_local_directory.up.sql` and `.down.sql`.
- Modify `server/pkg/db/queries/workspace.sql` — `UpdateWorkspace` gains the column; new `ClearWorkspaceDefaultLocalDirectory`.
- Modify `server/internal/handler/workspace.go` — request/response field, validation, clear path.
- Modify `server/internal/handler/project_resource.go` (`resolveClaimProjectContext`) — synthesize the workspace default.
- Tests: `server/internal/handler/tmux_mode_test.go` (new), `server/internal/handler/workspace_default_local_directory_test.go` (new).

**Daemon (Go)**
- Modify `server/internal/daemon/client.go` — conditional capability advertisement.
- Modify `server/internal/daemon/local_directory.go` — `localDirectoryModeTmux`, `UsesTmux`, `ValidateExecutionMode`.
- Modify `server/internal/daemon/daemon.go` — lock skip in `acquireLocalDirectoryLockIfNeeded`; branch to `runTmuxTask` in `runTask`; `adoptTmuxSessions` call in `Run`.
- Create `server/internal/daemon/tmux_runner.go` — pure helpers, state file, run script, watch loop, `runTmuxTask`, adoption.
- Create `server/internal/daemon/tmux_exec.go` — `tmuxController` interface and `execTmux` implementation.
- Modify `server/pkg/agent/claude.go` — `BuildClaudeInteractiveArgs`; `server/pkg/agent/mcp_config.go` — exported `HasManagedMcpConfig`.
- Tests: `server/internal/daemon/tmux_capability_test.go`, `server/internal/daemon/tmux_runner_test.go`, `server/internal/daemon/tmux_exec_test.go`, additions to `server/internal/daemon/local_directory_test.go`, `server/pkg/agent/claude_interactive_test.go`.

**Frontend (TypeScript)**
- Modify `packages/core/types/project.ts`, `packages/core/types/workspace.ts` — mode union and workspace field.
- Modify `packages/core/api/schemas.ts` — `LocalDirectoryRefSchema`, `parseDefaultLocalDirectory`, `local_tmux_supported`.
- Modify `packages/core/api/client.ts` — `getWorkspace`/`updateWorkspace` parse the new field; `updateWorkspace` accepts it.
- Modify `packages/core/config/index.ts`, `packages/core/platform/auth-initializer.tsx` — `localTmuxSupported`.
- Modify `packages/core/runtimes/cli-version.ts` — `LOCAL_TMUX_CAPABILITY`, `runtimeAdvertisesCapability`, `runtimeAdvertisesLocalTmux`.
- Modify `packages/views/projects/components/local-directory-mode-dialog.tsx` and `project-resources-section.tsx` — third card and wiring.
- Create `packages/views/settings/components/default-local-directory-section.tsx`; modify `repositories-tab.tsx` to render it.
- Modify locales `packages/views/locales/{en,zh-Hans,ja,ko}/projects.json` and `settings.json`.
- Tests: `packages/core/api/schema.test.ts` (additions), `packages/core/runtimes/cli-version.test.ts` (additions or new), `packages/views/projects/components/local-directory-mode-dialog.test.tsx` (additions), `packages/views/settings/components/default-local-directory-section.test.tsx` (new).

**Docs**
- Modify `server/internal/service/builtin_skills/multica-runtimes-and-repos/SKILL.md` and `references/runtimes-and-repos-source-map.md`.
- Modify the spec's failure-reason sentence (Task 15) to match the implementation.

---

### Task 1: Protocol capability and conditional daemon advertisement

**Files:**
- Modify: `server/pkg/protocol/messages.go` (after the `DaemonCapabilityLocalWorktreeV1` constant, line 22)
- Modify: `server/internal/daemon/client.go:184-200` (`daemonClientCapabilities`)
- Test: `server/internal/daemon/tmux_capability_test.go`

**Interfaces:**
- Produces: `protocol.DaemonCapabilityLocalTmuxV1 = "local-tmux-v1"`; daemon package var `tmuxLookPath func() (string, error)`; `func tmuxAvailable() bool`.

- [ ] **Step 1: Write the failing test**

```go
// server/internal/daemon/tmux_capability_test.go
package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The capability is what lets the server hand this daemon a tmux-mode task, so
// it must track the binary, not the build: a daemon without tmux on PATH must
// never claim to support the mode.
func TestDaemonClientCapabilitiesAdvertiseTmuxOnlyWhenTmuxResolves(t *testing.T) {
	orig := tmuxLookPath
	t.Cleanup(func() { tmuxLookPath = orig })

	tmuxLookPath = func() (string, error) { return "", errors.New("tmux: not found") }
	if strings.Contains(daemonClientCapabilities(), protocol.DaemonCapabilityLocalTmuxV1) {
		t.Fatal("daemon advertised local-tmux-v1 without a tmux binary")
	}

	tmuxLookPath = func() (string, error) { return "/opt/homebrew/bin/tmux", nil }
	if !strings.Contains(daemonClientCapabilities(), protocol.DaemonCapabilityLocalTmuxV1) {
		t.Fatal("daemon did not advertise local-tmux-v1 although tmux resolves")
	}
	// The pre-existing capabilities must survive the change.
	if !strings.Contains(daemonClientCapabilities(), protocol.DaemonCapabilityLocalWorktreeV1) {
		t.Fatal("local-worktree-v1 disappeared from the capability list")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `$GOTEST 'go test ./internal/daemon -run TestDaemonClientCapabilitiesAdvertiseTmux -count=1'`
Expected: FAIL to compile with `undefined: tmuxLookPath` and `undefined: protocol.DaemonCapabilityLocalTmuxV1`.

- [ ] **Step 3: Add the constant**

In `server/pkg/protocol/messages.go`, directly after `DaemonCapabilityLocalWorktreeV1 = "local-worktree-v1"`:

```go
	// DaemonCapabilityLocalTmuxV1 advertises that the daemon implements tmux
	// mode for local_directory resources (execution_mode=tmux): it opens an
	// interactive Claude Code session in a tmux session inside the folder and
	// completes the task when that session ends.
	//
	// A capability rather than a version check for the same reason as
	// worktree: a daemon that lacks the implementation json-skips the mode and
	// would run the task headlessly in place. The daemon only advertises this
	// when a tmux binary resolves on its PATH at startup, so a machine without
	// tmux can never be handed the mode.
	DaemonCapabilityLocalTmuxV1 = "local-tmux-v1"
```

- [ ] **Step 4: Advertise conditionally**

In `server/internal/daemon/client.go`, replace `daemonClientCapabilities` with:

```go
// tmuxLookPath resolves the tmux binary on the daemon's PATH. A package-level
// indirection so tests can force "present" and "absent" without a real tmux.
var tmuxLookPath = func() (string, error) { return exec.LookPath("tmux") }

// tmuxAvailable reports whether this daemon can run local_directory tasks in
// tmux mode. Re-checked on every advertisement (one PATH scan) so installing
// tmux and restarting the daemon is enough to unlock the mode.
func tmuxAvailable() bool {
	_, err := tmuxLookPath()
	return err == nil
}

// daemonClientCapabilities is the X-Client-Capabilities value the daemon
// advertises on BOTH the HTTP control-plane requests and the WS handshake, so a
// claim built over WS gets the same capability gating (skill refs,
// coalesced-comments) as the HTTP path. rpc-v1 advertises WS request/response
// support (MUL-4257). local-tmux-v1 is conditional: see tmuxAvailable.
func daemonClientCapabilities() string {
	caps := []string{
		protocol.DaemonCapabilitySkillBundlesV1,
		protocol.DaemonCapabilityCoalescedCommentsV1,
		protocol.DaemonCapabilityExecutionManifestV1,
		protocol.DaemonCapabilityAgentSkillV1,
		protocol.DaemonCapabilityRemoteMCPV1,
		protocol.DaemonCapabilityLocalWorktreeV1,
		protocol.DaemonCapabilitySourceContextQuickCreateV1,
		protocol.DaemonCapabilityRPCV1,
	}
	if tmuxAvailable() {
		caps = append(caps, protocol.DaemonCapabilityLocalTmuxV1)
	}
	return strings.Join(caps, ",")
}
```

Add `"os/exec"` to the import block of `client.go` if it is not already imported.

- [ ] **Step 5: Run test to verify it passes**

Run: `$GOTEST 'gofmt -l ./internal/daemon ./pkg/protocol; go vet ./internal/daemon ./pkg/protocol && go test ./internal/daemon -run TestDaemonClientCapabilitiesAdvertiseTmux -count=1 -v'`
Expected: `gofmt -l` prints nothing; `--- PASS`.

- [ ] **Step 6: Commit**

```bash
git add server/pkg/protocol/messages.go server/internal/daemon/client.go server/internal/daemon/tmux_capability_test.go
git commit -m "feat(daemon): advertise local-tmux-v1 when tmux is on PATH"
```

---

### Task 2: Server accepts `tmux` and gates it at save and claim time

**Files:**
- Modify: `server/internal/handler/project_resource.go:116-131` (constants), `:163-235` (save gate), `:271-305` (`validateLocalDirectoryRef`), call sites `:523` and `:643`
- Modify: `server/internal/handler/daemon.go:3138-3160` (claim gate call), `:3208-3235` (add `tmuxClaimBlockReason` after `worktreeClaimBlockReason`)
- Test: `server/internal/handler/tmux_mode_test.go`

**Interfaces:**
- Consumes: `protocol.DaemonCapabilityLocalTmuxV1` (Task 1); existing helpers `localDirRef(t, path, daemonID, mode)` and `runtimeWithVersion(daemonID, cliVersion)` from `worktree_claim_gate_test.go`; `runtimeHasCapability(metadata []byte, capability string) bool` (daemon.go:1482).
- Produces: `localDirectoryModeTmux = "tmux"`; `func localDirectoryModeCapability(mode string) (capability string, gated bool)`; `func daemonAdvertisesCapability(runtimes []db.AgentRuntime, daemonID, capability string) bool`; `func (h *Handler) requireModeCapableDaemon(w, r, workspaceID, resourceType, normalizedRef) bool`; `func tmuxClaimBlockReason(resources []ProjectResourceData, runtime db.AgentRuntime, hasTmuxCapability bool) string`.

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/handler/tmux_mode_test.go
package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func runtimeWithCapabilities(daemonID string, caps ...string) db.AgentRuntime {
	rt := db.AgentRuntime{DaemonID: pgtype.Text{String: daemonID, Valid: daemonID != ""}}
	rt.Metadata, _ = json.Marshal(map[string]any{"cli_version": "0.4.37", "capabilities": caps})
	return rt
}

func TestValidateLocalDirectoryRefAcceptsTmuxMode(t *testing.T) {
	out, err := validateLocalDirectoryRef(localDirRef(t, "/Users/dev/game", "daemon-a", "tmux"))
	if err != nil {
		t.Fatalf("tmux mode rejected: %v", err)
	}
	var ref localDirectoryRef
	if err := json.Unmarshal(out, &ref); err != nil {
		t.Fatalf("unmarshal normalized ref: %v", err)
	}
	if ref.ExecutionMode != localDirectoryModeTmux {
		t.Fatalf("execution_mode = %q, want %q", ref.ExecutionMode, localDirectoryModeTmux)
	}
	if _, err := validateLocalDirectoryRef(localDirRef(t, "/Users/dev/game", "daemon-a", "screen")); err == nil {
		t.Fatal("unknown mode was accepted")
	}
}

func TestLocalDirectoryModeCapability(t *testing.T) {
	cases := map[string]struct {
		capability string
		gated      bool
	}{
		"":         {"", false},
		"in_place": {"", false},
		"worktree": {protocol.DaemonCapabilityLocalWorktreeV1, true},
		"tmux":     {protocol.DaemonCapabilityLocalTmuxV1, true},
	}
	for mode, want := range cases {
		gotCap, gotGated := localDirectoryModeCapability(mode)
		if gotCap != want.capability || gotGated != want.gated {
			t.Errorf("mode %q: got (%q, %v), want (%q, %v)", mode, gotCap, gotGated, want.capability, want.gated)
		}
	}
}

func TestDaemonAdvertisesCapabilityReadsNewestRuntimeRow(t *testing.T) {
	const daemon = "daemon-a"
	old := runtimeWithCapabilities(daemon)
	newer := runtimeWithCapabilities(daemon, protocol.DaemonCapabilityLocalTmuxV1)
	newer.LastSeenAt = pgtype.Timestamptz{Time: old.LastSeenAt.Time.AddDate(0, 0, 1), Valid: true}
	if !daemonAdvertisesCapability([]db.AgentRuntime{old, newer}, daemon, protocol.DaemonCapabilityLocalTmuxV1) {
		t.Fatal("newest row advertises tmux but the check said no")
	}
	if daemonAdvertisesCapability([]db.AgentRuntime{old}, daemon, protocol.DaemonCapabilityLocalTmuxV1) {
		t.Fatal("a row without the capability was accepted")
	}
	if daemonAdvertisesCapability([]db.AgentRuntime{newer}, "other-daemon", protocol.DaemonCapabilityLocalTmuxV1) {
		t.Fatal("a different daemon's row was accepted")
	}
	// The worktree wrapper keeps its contract.
	if !daemonAdvertisesWorktree([]db.AgentRuntime{runtimeWithCapabilities(daemon, protocol.DaemonCapabilityLocalWorktreeV1)}, daemon) {
		t.Fatal("daemonAdvertisesWorktree regressed")
	}
}

func TestTmuxClaimBlockReason(t *testing.T) {
	const daemon = "daemon-a"
	tmuxRes := []ProjectResourceData{{
		ID: "r1", ResourceType: "local_directory",
		ResourceRef: localDirRef(t, "/Users/dev/game", daemon, "tmux"),
	}}

	t.Run("blocks a runtime that did not send the capability", func(t *testing.T) {
		reason := tmuxClaimBlockReason(tmuxRes, runtimeWithVersion(daemon, "0.4.37"), false)
		if reason == "" {
			t.Fatal("a runtime without tmux support was allowed to claim a tmux task")
		}
		if !strings.Contains(reason, "/Users/dev/game") || !strings.Contains(reason, "tmux") {
			t.Errorf("reason should name the directory and tmux, got: %q", reason)
		}
	})
	t.Run("lets a capable runtime through", func(t *testing.T) {
		if reason := tmuxClaimBlockReason(tmuxRes, runtimeWithVersion(daemon, "0.4.37"), true); reason != "" {
			t.Fatalf("capable runtime blocked: %q", reason)
		}
	})
	t.Run("ignores resources pinned to another daemon", func(t *testing.T) {
		other := []ProjectResourceData{{ID: "r2", ResourceType: "local_directory", ResourceRef: localDirRef(t, "/srv/x", "daemon-b", "tmux")}}
		if reason := tmuxClaimBlockReason(other, runtimeWithVersion(daemon, "0.4.37"), false); reason != "" {
			t.Fatalf("another daemon's resource blocked this claim: %q", reason)
		}
	})
	t.Run("ignores in_place and worktree resources", func(t *testing.T) {
		res := []ProjectResourceData{{ID: "r3", ResourceType: "local_directory", ResourceRef: localDirRef(t, "/srv/y", daemon, "worktree")}}
		if reason := tmuxClaimBlockReason(res, runtimeWithVersion(daemon, "0.4.37"), false); reason != "" {
			t.Fatalf("worktree resource triggered the tmux gate: %q", reason)
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `$GOTEST 'go test ./internal/handler -run "TestValidateLocalDirectoryRefAcceptsTmuxMode|TestLocalDirectoryModeCapability|TestDaemonAdvertisesCapability|TestTmuxClaimBlockReason" -count=1'`
Expected: FAIL to compile with `undefined: localDirectoryModeTmux`, `undefined: localDirectoryModeCapability`, `undefined: daemonAdvertisesCapability`, `undefined: tmuxClaimBlockReason`.

- [ ] **Step 3: Add the mode constant and accept it in validation**

In `project_resource.go`, extend the constant block after `localDirectoryModeWorktree`:

```go
	// localDirectoryModeTmux runs each task as an interactive Claude Code
	// session inside a tmux session in the user's directory. Tasks on the same
	// directory run concurrently in separate sessions and complete when their
	// session ends. Fork-only mode (ContextPRO); gated on the daemon capability
	// protocol.DaemonCapabilityLocalTmuxV1 because a daemon without the
	// implementation would otherwise run the task headlessly in place.
	localDirectoryModeTmux = "tmux"
```

In `validateLocalDirectoryRef`, change the switch to:

```go
	switch payload.ExecutionMode {
	case "", localDirectoryModeInPlace, localDirectoryModeWorktree, localDirectoryModeTmux:
	default:
		return nil, fmt.Errorf("local_directory: execution_mode must be %q, %q or %q, got %q",
			localDirectoryModeInPlace, localDirectoryModeWorktree, localDirectoryModeTmux, payload.ExecutionMode)
	}
```

- [ ] **Step 4: Generalize the save-time gate**

Replace `requireWorktreeCapableDaemon` and `daemonAdvertisesWorktree` (project_resource.go:163-235) with:

```go
// localDirectoryModeCapability maps an execution mode to the daemon capability
// that proves the mode is implemented. in_place is the historical default and
// needs no proof; the other two fail closed without their capability.
func localDirectoryModeCapability(mode string) (capability string, gated bool) {
	switch strings.TrimSpace(mode) {
	case localDirectoryModeWorktree:
		return protocol.DaemonCapabilityLocalWorktreeV1, true
	case localDirectoryModeTmux:
		return protocol.DaemonCapabilityLocalTmuxV1, true
	default:
		return "", false
	}
}

// requireModeCapableDaemon rejects saving a local_directory ref whose
// execution_mode needs a capability the owning daemon's newest runtime row does
// not advertise. It writes the 422 itself and returns false; true means "go on".
// Both gated modes share the shape because both fail the same way when skipped:
// the daemon would silently run in place.
func (h *Handler) requireModeCapableDaemon(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, resourceType string, normalizedRef json.RawMessage) bool {
	if resourceType != "local_directory" {
		return true
	}
	var ref localDirectoryRef
	if err := json.Unmarshal(normalizedRef, &ref); err != nil {
		return true
	}
	capability, gated := localDirectoryModeCapability(ref.ExecutionMode)
	if !gated {
		return true
	}
	runtimes, err := h.Queries.ListAgentRuntimes(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check runtime capabilities")
		return false
	}
	if daemonAdvertisesCapability(runtimes, ref.DaemonID, capability) {
		return true
	}
	if ref.ExecutionMode == localDirectoryModeTmux {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": fmt.Sprintf(
				"local_directory: %q is set to interactive terminal (tmux) mode, but the ContextPRO runtime on that machine does not advertise tmux support. Install tmux on that machine and restart the ContextPRO runtime, or pick another mode.",
				ref.LocalPath),
			"code":            "daemon_capability_missing",
			"capability":      capability,
			"current_version": latestDaemonCLIVersion(runtimes, ref.DaemonID),
			"daemon_id":       ref.DaemonID,
		})
		return false
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error": fmt.Sprintf(
			"local_directory: %q is set to parallel (worktree) mode, but the ContextPRO runtime on that machine does not support it. Update the ContextPRO app on that machine to the latest version, or keep the resource on in_place.",
			ref.LocalPath),
		"code":            "daemon_version_unsupported",
		"current_version": latestDaemonCLIVersion(runtimes, ref.DaemonID),
		"min_version":     agentpkg.MinLocalWorktreeCLIVersion,
		"daemon_id":       ref.DaemonID,
	})
	return false
}

// daemonAdvertisesCapability reports whether the NEWEST runtime row registered
// by daemonID advertises capability. Newest, not any: a daemon re-registers on
// every start, so an old row that still lists a capability must not outvote the
// current binary that dropped it.
func daemonAdvertisesCapability(runtimes []db.AgentRuntime, daemonID, capability string) bool {
	if strings.TrimSpace(daemonID) == "" {
		return false
	}
	var newest *db.AgentRuntime
	for i := range runtimes {
		rt := &runtimes[i]
		if !rt.DaemonID.Valid || rt.DaemonID.String != daemonID {
			continue
		}
		if newest == nil || runtimeSeenAfter(rt, newest) {
			newest = rt
		}
	}
	if newest == nil {
		return false
	}
	return runtimeHasCapability(newest.Metadata, capability)
}

// daemonAdvertisesWorktree is kept for the existing worktree tests and callers.
func daemonAdvertisesWorktree(runtimes []db.AgentRuntime, daemonID string) bool {
	return daemonAdvertisesCapability(runtimes, daemonID, protocol.DaemonCapabilityLocalWorktreeV1)
}
```

Update the two call sites (`:523` in `CreateProjectResource`, `:643` in `UpdateProjectResource`) from `h.requireWorktreeCapableDaemon(` to `h.requireModeCapableDaemon(`. Run `grep -n requireWorktreeCapableDaemon server/internal/handler/*.go` and expect no matches.

- [ ] **Step 5: Add the claim-time gate**

In `daemon.go`, directly after `worktreeClaimBlockReason` (ends about line 3235) add:

```go
// tmuxClaimBlockReason mirrors worktreeClaimBlockReason for tmux mode: a
// daemon that did not advertise local-tmux-v1 in X-Client-Capabilities must not
// receive a tmux-mode local_directory task, because it would run it headlessly
// in place. Empty string means "no objection".
func tmuxClaimBlockReason(resources []ProjectResourceData, runtime db.AgentRuntime, hasTmuxCapability bool) string {
	if !runtime.DaemonID.Valid || runtime.DaemonID.String == "" {
		return ""
	}
	if hasTmuxCapability {
		return ""
	}
	for _, res := range resources {
		if res.ResourceType != "local_directory" {
			continue
		}
		var ref localDirectoryRef
		if err := json.Unmarshal(res.ResourceRef, &ref); err != nil {
			continue
		}
		if ref.ExecutionMode != localDirectoryModeTmux || ref.DaemonID != runtime.DaemonID.String {
			continue
		}
		return fmt.Sprintf(
			"This machine's ContextPRO runtime does not advertise tmux support, which %q is set to use (interactive terminal mode). "+
				"Install tmux on that machine and restart the ContextPRO runtime, then re-run this task. "+
				"Refusing to run rather than falling back to a headless run in the directory.",
			ref.LocalPath)
	}
	return ""
}
```

Then change the gate call at daemon.go:3138-3142 so both gates feed the same cancel block:

```go
	reason := worktreeClaimBlockReason(
		resp.ProjectResources,
		runtime,
		requestHasClientCapability(r, protocol.DaemonCapabilityLocalWorktreeV1),
	)
	if reason == "" {
		reason = tmuxClaimBlockReason(
			resp.ProjectResources,
			runtime,
			requestHasClientCapability(r, protocol.DaemonCapabilityLocalTmuxV1),
		)
	}
	if reason != "" {
		// (existing body: slog.Error + CancelTaskWithReason + requeue, unchanged)
```

Keep the existing log/cancel body; only the `if reason := ...; reason != ""` header changes into the two-step form above. The log message "runtime too old for worktree mode" becomes "runtime lacks the capability for this local_directory mode".

- [ ] **Step 6: Run tests to verify they pass, plus the existing worktree gate tests**

Run: `$GOTEST 'gofmt -l ./internal/handler; go vet ./internal/handler && go test ./internal/handler -run "TestValidateLocalDirectoryRefAcceptsTmuxMode|TestLocalDirectoryModeCapability|TestDaemonAdvertisesCapability|TestTmuxClaimBlockReason|TestWorktreeClaimBlockReason|LocalDirectory" -count=1 -v 2>&1 | grep -E "^(--- |ok|FAIL)"'`
Expected: all `--- PASS`, `ok`.

- [ ] **Step 7: Commit**

```bash
git add server/internal/handler/project_resource.go server/internal/handler/daemon.go server/internal/handler/tmux_mode_test.go
git commit -m "feat(server): accept tmux execution mode and gate it on local-tmux-v1"
```

---

### Task 3: Server config flag and client store for tmux support

**Files:**
- Modify: `server/internal/handler/config.go:76` (add field after `LocalWorktreeSupported`), `:100` (set true)
- Modify: `packages/core/api/schemas.ts:752` (interface `AppConfigResponse`), `:967` (defaults object)
- Modify: `packages/core/config/index.ts` (state field, default, setter)
- Modify: `packages/core/platform/auth-initializer.tsx:86-88` (call the setter)
- Test: `packages/core/config/local-tmux-supported.test.ts` (new)

**Interfaces:**
- Produces: JSON field `local_tmux_supported: boolean` on `/api/config`; store field `localTmuxSupported: boolean` and `setLocalTmuxSupported(supported?: boolean)`.

- [ ] **Step 1: Write the failing test**

```ts
// packages/core/config/local-tmux-supported.test.ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { useConfigStore } from "./index";

describe("localTmuxSupported", () => {
  it("defaults to false and only accepts an explicit true", () => {
    expect(useConfigStore.getState().localTmuxSupported).toBe(false);
    useConfigStore.getState().setLocalTmuxSupported(true);
    expect(useConfigStore.getState().localTmuxSupported).toBe(true);
    // An absent or non-boolean value from an old server must fail closed.
    useConfigStore.getState().setLocalTmuxSupported(undefined);
    expect(useConfigStore.getState().localTmuxSupported).toBe(false);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd packages/core && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run config/local-tmux-supported.test.ts`
Expected: FAIL, `setLocalTmuxSupported is not a function`.

- [ ] **Step 3: Server flag**

In `server/internal/handler/config.go`, after the `LocalWorktreeSupported bool` field:

```go
	// LocalTmuxSupported tells clients this server understands
	// execution_mode=tmux on local_directory resources and gates it on the
	// runtime's local-tmux-v1 capability at save time. Fork-only (ContextPRO).
	// Absent must read as "cannot honour it", like LocalWorktreeSupported.
	LocalTmuxSupported bool `json:"local_tmux_supported"`
```

and in the response literal next to `LocalWorktreeSupported: true,` add `LocalTmuxSupported: true,`.

- [ ] **Step 4: Client type, defaults, store, initializer**

`packages/core/api/schemas.ts`: after `local_worktree_supported?: boolean;` add
```ts
  /** Whether this server understands execution_mode=tmux and gates it on the
   * runtime capability. Fork-only; absent fails closed like worktree. */
  local_tmux_supported?: boolean;
```
and in the defaults object after `local_worktree_supported: false,` add `local_tmux_supported: false,`.

`packages/core/config/index.ts`: after `localWorktreeSupported: boolean;` add
```ts
  // Whether the connected server validates execution_mode=tmux. Same
  // fail-closed rule as localWorktreeSupported.
  localTmuxSupported: boolean;
```
after the `setLocalWorktreeSupported` signature in the store type add
```ts
  setLocalTmuxSupported: (supported?: boolean) => void;
```
in the initial state add `localTmuxSupported: false,` and next to `setLocalWorktreeSupported` add
```ts
  setLocalTmuxSupported: (supported = false) =>
    set({ localTmuxSupported: supported === true }),
```

`packages/core/platform/auth-initializer.tsx`, after the `setLocalWorktreeSupported` call:
```ts
        configStore
          .getState()
          .setLocalTmuxSupported(cfg.local_tmux_supported === true);
```

- [ ] **Step 5: Run test and typecheck**

Run: `cd packages/core && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run config/local-tmux-supported.test.ts && pnpm typecheck`
Expected: PASS; typecheck clean. Also `$GOTEST 'go vet ./internal/handler && go test ./internal/handler -run TestGetConfig -count=1'` → ok.

- [ ] **Step 6: Commit**

```bash
git add server/internal/handler/config.go packages/core/api/schemas.ts packages/core/config/index.ts packages/core/platform/auth-initializer.tsx packages/core/config/local-tmux-supported.test.ts
git commit -m "feat(config): expose local_tmux_supported to clients"
```

---
### Task 4: Workspace default folder — migration, sqlc, and workspace API

**Files:**
- Create: `server/migrations/444_workspace_default_local_directory.up.sql`, `server/migrations/444_workspace_default_local_directory.down.sql`
- Modify: `server/pkg/db/queries/workspace.sql:47-58` (`UpdateWorkspace`), append `ClearWorkspaceDefaultLocalDirectory`
- Modify: `server/internal/handler/workspace.go:97-135` (`WorkspaceResponse`, `workspaceToResponse`), `:320-332` (`UpdateWorkspaceRequest`), `:373-450` (`UpdateWorkspace`)
- Test: `server/internal/handler/workspace_default_local_directory_test.go`

**Interfaces:**
- Consumes: `validateLocalDirectoryRef(ref json.RawMessage) (json.RawMessage, error)` and `requireModeCapableDaemon` (Task 2); test helpers `newRequest`, `withURLParam`, `testWorkspaceID`, `testHandler`, `testutil.Call(t, h, req).Want(status).JSON(&out)`.
- Produces: generated `db.Workspace.DefaultLocalDirectory []byte`, `db.UpdateWorkspaceParams.DefaultLocalDirectory []byte`, `Queries.ClearWorkspaceDefaultLocalDirectory(ctx, id) error`; JSON field `default_local_directory` (object or null) on workspace GET/PUT.

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/handler/workspace_default_local_directory_test.go
package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func clearWorkspaceDefaultLocalDirectory(t *testing.T) {
	t.Helper()
	req := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{"default_local_directory": nil})
	req = withURLParam(req, "id", testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK)
}

func getWorkspaceResponse(t *testing.T) WorkspaceResponse {
	t.Helper()
	req := withURLParam(newRequest("GET", "/api/workspaces/"+testWorkspaceID, nil), "id", testWorkspaceID)
	var out WorkspaceResponse
	testutil.Call(t, testHandler.GetWorkspace, req).Want(http.StatusOK).JSON(&out)
	return out
}

func TestUpdateWorkspaceDefaultLocalDirectoryRoundTrip(t *testing.T) {
	t.Cleanup(func() { clearWorkspaceDefaultLocalDirectory(t) })

	req := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{
		"default_local_directory": map[string]any{
			"local_path": "/Users/dev/contextpro", "daemon_id": "daemon-roundtrip",
			"execution_mode": "in_place", "label": "ContextPRO",
		},
	})
	req = withURLParam(req, "id", testWorkspaceID)
	var updated WorkspaceResponse
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK).JSON(&updated)
	got, ok := updated.DefaultLocalDirectory.(map[string]any)
	if !ok || got["local_path"] != "/Users/dev/contextpro" || got["execution_mode"] != "in_place" || got["label"] != "ContextPRO" {
		t.Fatalf("PUT response default_local_directory = %#v", updated.DefaultLocalDirectory)
	}

	if fetched := getWorkspaceResponse(t); fetched.DefaultLocalDirectory == nil {
		t.Fatal("GET dropped default_local_directory after it was saved")
	}

	clearWorkspaceDefaultLocalDirectory(t)
	if fetched := getWorkspaceResponse(t); fetched.DefaultLocalDirectory != nil {
		t.Fatalf("null did not clear the default, got %#v", fetched.DefaultLocalDirectory)
	}
}

func TestUpdateWorkspaceRejectsInvalidDefaultLocalDirectory(t *testing.T) {
	for name, ref := range map[string]map[string]any{
		"relative path": {"local_path": "relative/dir", "daemon_id": "d"},
		"missing daemon": {"local_path": "/srv/app"},
		"unknown mode":   {"local_path": "/srv/app", "daemon_id": "d", "execution_mode": "screen"},
	} {
		t.Run(name, func(t *testing.T) {
			req := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{"default_local_directory": ref})
			req = withURLParam(req, "id", testWorkspaceID)
			testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusBadRequest)
		})
	}
	if fetched := getWorkspaceResponse(t); fetched.DefaultLocalDirectory != nil {
		t.Fatalf("a rejected update still stored a default: %#v", fetched.DefaultLocalDirectory)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `$GOTEST 'go test ./internal/handler -run "TestUpdateWorkspaceDefaultLocalDirectoryRoundTrip|TestUpdateWorkspaceRejectsInvalidDefaultLocalDirectory" -count=1'`
Expected: FAIL to compile: `updated.DefaultLocalDirectory undefined`.

- [ ] **Step 3: Migration**

`server/migrations/444_workspace_default_local_directory.up.sql`:
```sql
-- Workspace-level default local directory (ContextPRO fork). Same JSONB ref
-- shape as a project_resource of type local_directory:
-- {local_path, daemon_id, label?, execution_mode?}. A project without its own
-- local_directory resource inherits it at claim time (handler
-- resolveClaimProjectContext). Sits next to repos / mcp_config on the workspace
-- row: read by primary key only, so no index; no foreign key to any runtime
-- (repo rule), the daemon_id is validated in application code.
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS default_local_directory JSONB;
```

`server/migrations/444_workspace_default_local_directory.down.sql`:
```sql
ALTER TABLE workspace DROP COLUMN IF EXISTS default_local_directory;
```

- [ ] **Step 4: Queries and sqlc**

In `server/pkg/db/queries/workspace.sql`, inside `UpdateWorkspace` add before `updated_at = now()`:
```sql
    default_local_directory = COALESCE(sqlc.narg('default_local_directory'), default_local_directory),
```
Append at the end of the file:
```sql
-- name: ClearWorkspaceDefaultLocalDirectory :exec
-- COALESCE in UpdateWorkspace cannot write NULL, so clearing is its own statement.
UPDATE workspace SET default_local_directory = NULL, updated_at = now()
WHERE id = $1;
```
Regenerate: `$GOTEST 'go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate && grep -n DefaultLocalDirectory pkg/db/generated/models.go pkg/db/generated/workspace.sql.go | head -4'`
Expected: the field appears in `Workspace` and `UpdateWorkspaceParams`, and `ClearWorkspaceDefaultLocalDirectory` exists.

- [ ] **Step 5: Handler**

`WorkspaceResponse`: add after `Repos any \`json:"repos"\``:
```go
	// DefaultLocalDirectory is the workspace-wide fallback folder for tasks
	// (ContextPRO fork). null when unset; otherwise the local_directory ref
	// shape {local_path, daemon_id, label?, execution_mode?}.
	DefaultLocalDirectory any `json:"default_local_directory"`
```
In `workspaceToResponse`, after the repos block:
```go
	var defaultLocalDirectory any
	if len(w.DefaultLocalDirectory) > 0 {
		json.Unmarshal(w.DefaultLocalDirectory, &defaultLocalDirectory)
	}
```
and set `DefaultLocalDirectory: defaultLocalDirectory,` in the literal.

`UpdateWorkspaceRequest`: add
```go
	// DefaultLocalDirectory distinguishes absent (nil: leave as is) from an
	// explicit JSON null (clear) from an object (validate and store).
	DefaultLocalDirectory *json.RawMessage `json:"default_local_directory"`
```

In `UpdateWorkspace`, after the `req.Repos` block and before the issue-prefix validation, add:
```go
	// Fork (ContextPRO): workspace default folder. Validated like a project
	// resource so the daemon can trust the ref shape, and gated on the runtime
	// capability for worktree/tmux exactly like a project resource save.
	clearDefaultLocalDirectory := false
	if req.DefaultLocalDirectory != nil {
		raw := bytes.TrimSpace(*req.DefaultLocalDirectory)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			clearDefaultLocalDirectory = true
		} else {
			normalized, err := validateLocalDirectoryRef(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			if !h.requireModeCapableDaemon(w, r, idUUID, "local_directory", normalized) {
				return
			}
			params.DefaultLocalDirectory = normalized
		}
	}
```
Immediately before `ws, err := h.Queries.UpdateWorkspace(r.Context(), params)` add:
```go
	if clearDefaultLocalDirectory {
		if err := h.Queries.ClearWorkspaceDefaultLocalDirectory(r.Context(), idUUID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to clear default local directory: "+err.Error())
			return
		}
	}
```
Add `"bytes"` to the imports of `workspace.go`.

- [ ] **Step 6: Run the tests**

Run: `$GOTEST 'go run ./cmd/migrate up >/dev/null; gofmt -l ./internal/handler; go vet ./internal/handler && go test ./internal/handler -run "TestUpdateWorkspace|TestGetWorkspace" -count=1 -v 2>&1 | grep -E "^(--- |ok|FAIL)"'`
Expected: all PASS including the pre-existing `TestUpdateWorkspace_AvatarURL` and `TestUpdateWorkspaceRejectsMalformedID`.

- [ ] **Step 7: Commit**

```bash
git add server/migrations/444_workspace_default_local_directory.up.sql server/migrations/444_workspace_default_local_directory.down.sql server/pkg/db/queries/workspace.sql server/pkg/db/generated server/internal/handler/workspace.go server/internal/handler/workspace_default_local_directory_test.go
git commit -m "feat(server): workspace default local directory"
```

---

### Task 5: Claim synthesizes the workspace default into the task's resources

**Files:**
- Modify: `server/internal/handler/project_resource.go:900-965` (`resolveClaimProjectContext`)
- Test: append to `server/internal/handler/workspace_default_local_directory_test.go`

**Interfaces:**
- Consumes: `db.Workspace.DefaultLocalDirectory []byte` (Task 4); `localDirectoryRefLabel(ref json.RawMessage) string` (existing, :306); `parseUUID(s string) pgtype.UUID` (trusted round-trip, handler.go:579).
- Produces: `const workspaceDefaultLocalDirectoryResourceID = "workspace-default"`; `func hasLocalDirectoryResource(resources []ProjectResourceData) bool`.

- [ ] **Step 1: Write the failing test**

Append to the test file:

```go
func TestResolveClaimProjectContextSynthesizesWorkspaceDefault(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { clearWorkspaceDefaultLocalDirectory(t) })

	// A project with no resources of its own.
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{"title": "tmux default folder project"})
	var project ProjectResponse
	testutil.Call(t, testHandler.CreateProject, req).Want(http.StatusCreated).JSON(&project)
	t.Cleanup(func() {
		del := withURLParam(newRequest("DELETE", "/api/projects/"+project.ID, nil), "id", project.ID)
		testHandler.DeleteProject(httptest.NewRecorder(), del)
	})

	// No default yet: no local_directory resource is attached.
	out, err := testHandler.resolveClaimProjectContext(ctx, parseUUID(project.ID), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("resolve without default: %v", err)
	}
	if hasLocalDirectoryResource(out.Resources) {
		t.Fatalf("no default configured but a local_directory resource appeared: %+v", out.Resources)
	}

	// Workspace default set: it is synthesized as a resource.
	setReq := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{
		"default_local_directory": map[string]any{"local_path": "/Users/dev/default", "daemon_id": "daemon-default", "execution_mode": "in_place"},
	})
	testutil.Call(t, testHandler.UpdateWorkspace, withURLParam(setReq, "id", testWorkspaceID)).Want(http.StatusOK)

	out, err = testHandler.resolveClaimProjectContext(ctx, parseUUID(project.ID), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("resolve with default: %v", err)
	}
	var synthesized *ProjectResourceData
	for i := range out.Resources {
		if out.Resources[i].ResourceType == "local_directory" {
			synthesized = &out.Resources[i]
		}
	}
	if synthesized == nil || synthesized.ID != workspaceDefaultLocalDirectoryResourceID {
		t.Fatalf("workspace default not synthesized: %+v", out.Resources)
	}
	var ref localDirectoryRef
	if err := json.Unmarshal(synthesized.ResourceRef, &ref); err != nil || ref.LocalPath != "/Users/dev/default" || ref.DaemonID != "daemon-default" {
		t.Fatalf("synthesized ref = %s (%v)", synthesized.ResourceRef, err)
	}

	// A project resource wins over the default.
	resReq := newRequest("POST", "/api/projects/"+project.ID+"/resources", map[string]any{
		"resource_type": "local_directory",
		"resource_ref":  map[string]any{"local_path": "/Users/dev/project-own", "daemon_id": "daemon-own", "execution_mode": "in_place"},
	})
	testutil.Call(t, testHandler.CreateProjectResource, withURLParam(resReq, "id", project.ID)).Want(http.StatusCreated)

	out, err = testHandler.resolveClaimProjectContext(ctx, parseUUID(project.ID), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("resolve with project resource: %v", err)
	}
	var localDirs int
	for _, res := range out.Resources {
		if res.ResourceType != "local_directory" {
			continue
		}
		localDirs++
		if res.ID == workspaceDefaultLocalDirectoryResourceID {
			t.Fatal("workspace default was attached although the project has its own local_directory resource")
		}
	}
	if localDirs != 1 {
		t.Fatalf("expected exactly one local_directory resource, got %d", localDirs)
	}
}
```

Add `"context"`, `"encoding/json"`, `"net/http/httptest"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `$GOTEST 'go test ./internal/handler -run TestResolveClaimProjectContextSynthesizesWorkspaceDefault -count=1'`
Expected: FAIL to compile: `undefined: hasLocalDirectoryResource`, `undefined: workspaceDefaultLocalDirectoryResourceID`.

- [ ] **Step 3: Implement**

In `project_resource.go`, above `resolveClaimProjectContext`:

```go
// workspaceDefaultLocalDirectoryResourceID marks a resource the claim handler
// synthesized from workspace.default_local_directory. It is not a
// project_resource row id; the daemon treats it like any other local_directory
// resource and only logs it.
const workspaceDefaultLocalDirectoryResourceID = "workspace-default"

func hasLocalDirectoryResource(resources []ProjectResourceData) bool {
	for _, res := range resources {
		if res.ResourceType == "local_directory" {
			return true
		}
	}
	return false
}
```

Rewrite the tail of `resolveClaimProjectContext` (from `if len(out.Repos) > 0 {` to the end of the repos block) as:

```go
	needWorkspaceRepos := len(out.Repos) == 0
	// Fork (ContextPRO): only a project can inherit the workspace default
	// folder, and only when it has no local_directory resource of its own.
	needDefaultFolder := out.ProjectID != "" && !hasLocalDirectoryResource(out.Resources)
	if !needWorkspaceRepos && !needDefaultFolder {
		return out, nil
	}
	ws, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return claimProjectContext{}, fmt.Errorf("get workspace: %w", err)
	}
	if needWorkspaceRepos && ws.Repos != nil {
		var repos []RepoData
		if jsonErr := json.Unmarshal(ws.Repos, &repos); jsonErr != nil {
			slog.Error("claim project context: workspace repos are not valid JSON; claiming without repos",
				"workspace_id", uuidToString(workspaceID), "error", jsonErr)
		} else if len(repos) > 0 {
			out.Repos = repos
		}
	}
	if needDefaultFolder && len(ws.DefaultLocalDirectory) > 0 && !bytes.Equal(bytes.TrimSpace(ws.DefaultLocalDirectory), []byte("null")) {
		out.Resources = append(out.Resources, ProjectResourceData{
			ID:           workspaceDefaultLocalDirectoryResourceID,
			ResourceType: "local_directory",
			ResourceRef:  json.RawMessage(ws.DefaultLocalDirectory),
			Label:        localDirectoryRefLabel(ws.DefaultLocalDirectory),
		})
	}
	return out, nil
```

Keep whatever the function returned after the repos block before (it ended with `return out, nil`). Add `"bytes"` to the imports if missing.

- [ ] **Step 4: Run tests**

Run: `$GOTEST 'gofmt -l ./internal/handler; go vet ./internal/handler && go test ./internal/handler -run "TestResolveClaimProjectContext|Claim" -count=1 2>&1 | grep -E "^(--- FAIL|ok|FAIL)"'`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler/project_resource.go server/internal/handler/workspace_default_local_directory_test.go
git commit -m "feat(server): attach the workspace default folder to claims without a project folder"
```

---

### Task 6: Daemon accepts the mode and skips the folder mutex for it

**Files:**
- Modify: `server/internal/daemon/local_directory.go:22-30` (constants), `:53-60` (add `UsesTmux`), `:82-108` (`ValidateExecutionMode`)
- Modify: `server/internal/daemon/daemon.go:5335-5380` (`acquireLocalDirectoryLockIfNeeded`, after the `UsesWorktree` branch)
- Test: append to `server/internal/daemon/local_directory_test.go`

**Interfaces:**
- Produces: `localDirectoryModeTmux = "tmux"`; `func (a *localDirectoryAssignment) UsesTmux() bool`.

- [ ] **Step 1: Write the failing tests**

```go
func TestValidateExecutionModeAcceptsTmux(t *testing.T) {
	t.Parallel()
	a := &localDirectoryAssignment{Ref: localDirectoryRef{LocalPath: "/srv/app", DaemonID: "d", ExecutionMode: "tmux"}, AbsPath: "/srv/app"}
	if err := a.ValidateExecutionMode(); err != nil {
		t.Fatalf("tmux rejected: %v", err)
	}
	if !a.UsesTmux() || a.UsesWorktree() {
		t.Fatalf("UsesTmux=%v UsesWorktree=%v, want true/false", a.UsesTmux(), a.UsesWorktree())
	}
	bad := &localDirectoryAssignment{Ref: localDirectoryRef{LocalPath: "/srv/app", DaemonID: "d", ExecutionMode: "screen"}, AbsPath: "/srv/app"}
	if err := bad.ValidateExecutionMode(); err == nil || !strings.Contains(err.Error(), `"tmux"`) {
		t.Fatalf("unknown mode error should list tmux as a valid option, got %v", err)
	}
}

// tmux tasks run side by side in one folder by design (each is its own visible
// terminal), so they must not queue on the per-path mutex.
func TestAcquireLocalDirectoryLockSkipsTmuxTasks(t *testing.T) {
	t.Parallel()
	const daemonID = "d-tmux"
	tmp := t.TempDir()
	raw, err := json.Marshal(localDirectoryRef{LocalPath: tmp, DaemonID: daemonID, ExecutionMode: "tmux"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	task := Task{ID: "tmux-task", ProjectResources: []ProjectResourceData{{ID: "r1", ResourceType: localDirectoryResourceType, ResourceRef: raw}}}
	d := &Daemon{cfg: Config{DaemonID: daemonID}, localPathLocks: NewLocalPathLocker(), logger: slog.Default()}

	release, abort := d.acquireLocalDirectoryLockIfNeeded(context.Background(), task, slog.Default())
	if abort {
		t.Fatal("tmux task aborted at lock acquisition")
	}
	if release != nil {
		t.Fatal("tmux task took the path mutex; it must run unserialised")
	}
	assignment, err := localDirectoryAssignmentForTask(task, daemonID)
	if err != nil || assignment == nil {
		t.Fatalf("assignment: %v %v", assignment, err)
	}
	if got := d.localPathLocks.Holder(assignment.RealPath); got != "" {
		t.Fatalf("holder after tmux skip = %q, want empty", got)
	}
}
```

Ensure `"strings"` is imported in the test file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `$GOTEST 'go test ./internal/daemon -run "TestValidateExecutionModeAcceptsTmux|TestAcquireLocalDirectoryLockSkipsTmuxTasks" -count=1'`
Expected: FAIL to compile: `a.UsesTmux undefined`; after adding a stub, `TestValidateExecutionModeAcceptsTmux` fails with "does not support execution_mode \"tmux\"".

- [ ] **Step 3: Implement**

`local_directory.go` constants:
```go
const (
	localDirectoryModeInPlace  = "in_place"
	localDirectoryModeWorktree = "worktree"
	// localDirectoryModeTmux: interactive Claude Code in a tmux session in the
	// user's folder (ContextPRO fork). Mirrors handler.localDirectoryModeTmux.
	localDirectoryModeTmux = "tmux"
)
```
After `UsesWorktree`:
```go
// UsesTmux reports whether this assignment runs the task as an interactive
// tmux session. Like worktree tasks, tmux tasks skip the per-path mutex — but
// for the opposite reason: they DO share the working copy, on purpose, because
// each one is a terminal the user watches and steers.
func (a *localDirectoryAssignment) UsesTmux() bool {
	return a != nil && strings.TrimSpace(a.Ref.ExecutionMode) == localDirectoryModeTmux
}
```
`ValidateExecutionMode`: the accepting case becomes `case "", localDirectoryModeInPlace, localDirectoryModeWorktree, localDirectoryModeTmux:` and the error text lists the three modes:
```go
		return fmt.Errorf(
			"local_directory: this daemon does not support execution_mode %q for %q "+
				"(update the daemon, or set the resource's execution mode to %q, %q or %q); "+
				"refusing to run in place, since that would modify a directory the resource asked to isolate",
			a.Ref.ExecutionMode, a.AbsPath, localDirectoryModeInPlace, localDirectoryModeWorktree, localDirectoryModeTmux)
```
`daemon.go` `acquireLocalDirectoryLockIfNeeded`, directly after the `if assignment.UsesWorktree() { ... return nil, false }` block:
```go
	if assignment.UsesTmux() {
		// Deliberate (design 2026-09-02): tmux tasks in one folder run side by
		// side, each in its own visible session. Serialising them would only
		// queue terminals the user is waiting to attach to.
		taskLog.Info("local_directory: tmux mode, skipping path mutex")
		return nil, false
	}
```

- [ ] **Step 4: Run tests**

Run: `$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "LocalDirectory|ExecutionMode" -count=1 2>&1 | grep -E "^(--- FAIL|ok|FAIL)"'`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/daemon/local_directory.go server/internal/daemon/daemon.go server/internal/daemon/local_directory_test.go
git commit -m "feat(daemon): accept tmux execution mode and exempt it from the folder mutex"
```

---

### Task 7: Interactive Claude Code argument builder

**Files:**
- Modify: `server/pkg/agent/mcp_config.go` (export `HasManagedMcpConfig`)
- Modify: `server/pkg/agent/claude.go` (after `buildClaudeArgs`, about line 770)
- Test: `server/pkg/agent/claude_interactive_test.go`

**Interfaces:**
- Consumes: `filterCustomArgs(args []string, blocked map[string]blockedArgMode, logger *slog.Logger) []string`, `blockedStandalone`, `blockedWithValue`, `hasManagedMcpConfig` (all existing in package agent).
- Produces: `func HasManagedMcpConfig(raw json.RawMessage) bool`; `func BuildClaudeInteractiveArgs(opts ExecOptions, mcpConfigPath string, logger *slog.Logger) []string`.

- [ ] **Step 1: Write the failing test**

```go
// server/pkg/agent/claude_interactive_test.go
package agent

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestBuildClaudeInteractiveArgsNeverEmitsHeadlessFlags(t *testing.T) {
	args := BuildClaudeInteractiveArgs(ExecOptions{
		Model:              "claude-opus-5",
		ThinkingLevel:      "high",
		ClaudeSettingsPath: "/tmp/settings.json",
		ExtraArgs:          []string{"--verbose"},
		CustomArgs:         []string{"-p", "--output-format", "json", "--permission-mode", "bypassPermissions", "--dangerously-skip-permissions", "--add-dir", "/extra"},
	}, "/tmp/task/mcp.json", slog.Default())

	joined := " " + strings.Join(args, " ") + " "
	for _, forbidden := range []string{" -p ", " --print ", " --output-format ", " --input-format ", " --permission-mode ", " --dangerously-skip-permissions ", " --disallowedTools ", " --resume "} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("interactive args contain headless flag %q: %v", strings.TrimSpace(forbidden), args)
		}
	}
	for _, want := range [][]string{{"--model", "claude-opus-5"}, {"--effort", "high"}, {"--mcp-config", "/tmp/task/mcp.json"}, {"--settings", "/tmp/settings.json"}, {"--add-dir", "/extra"}, {"--verbose"}} {
		if !containsSequence(args, want) {
			t.Errorf("missing %v in %v", want, args)
		}
	}
	if slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("--strict-mcp-config must only appear with a managed MCP config: %v", args)
	}
}

func TestBuildClaudeInteractiveArgsStrictMcpOnlyWhenManaged(t *testing.T) {
	managed := BuildClaudeInteractiveArgs(ExecOptions{McpConfig: json.RawMessage(`{"mcpServers":{}}`)}, "/tmp/mcp.json", slog.Default())
	if !slices.Contains(managed, "--strict-mcp-config") || !containsSequence(managed, []string{"--mcp-config", "/tmp/mcp.json"}) {
		t.Fatalf("managed config must pin --mcp-config and --strict-mcp-config: %v", managed)
	}
	none := BuildClaudeInteractiveArgs(ExecOptions{}, "", slog.Default())
	if len(none) != 0 {
		t.Fatalf("no options should produce no args, got %v", none)
	}
}

func containsSequence(haystack, needle []string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `$GOTEST 'go test ./pkg/agent -run TestBuildClaudeInteractiveArgs -count=1'`
Expected: FAIL to compile: `undefined: BuildClaudeInteractiveArgs`.

- [ ] **Step 3: Implement**

`mcp_config.go`, after `hasManagedMcpConfig`:
```go
// HasManagedMcpConfig is the exported form for the daemon's tmux runner, which
// writes the config to a file itself instead of going through Execute.
func HasManagedMcpConfig(raw json.RawMessage) bool { return hasManagedMcpConfig(raw) }
```

`claude.go`, after `buildClaudeArgs`:
```go
// claudeInteractiveBlockedArgs are the flags a user-supplied custom_args list
// may not smuggle into an INTERACTIVE launch. Everything that turns the session
// headless or removes the permission prompt is blocked: the whole point of tmux
// mode is a human watching and approving in the pane. Session selection flags
// are blocked too because the runner owns the conversation lifecycle.
var claudeInteractiveBlockedArgs = map[string]blockedArgMode{
	"-p":                             blockedStandalone,
	"--print":                        blockedStandalone,
	"--output-format":                blockedWithValue,
	"--input-format":                 blockedWithValue,
	"--permission-mode":              blockedWithValue,
	"--dangerously-skip-permissions": blockedStandalone,
	"--disallowedTools":              blockedWithValue,
	"--mcp-config":                   blockedWithValue,
	"--effort":                       blockedWithValue,
	"--resume":                       blockedWithValue,
	"--continue":                     blockedStandalone,
}

// BuildClaudeInteractiveArgs builds the argument list for an interactive Claude
// Code session (tmux mode). It is buildClaudeArgs minus every headless flag:
// no -p, no stream-json, no bypassPermissions, no AskUserQuestion ban. The
// prompt is NOT included; the caller appends it as the positional argument.
// mcpConfigPath is the file the caller wrote from opts.McpConfig ("" = none).
func BuildClaudeInteractiveArgs(opts ExecOptions, mcpConfigPath string, logger *slog.Logger) []string {
	var args []string
	if mcpConfigPath != "" {
		args = append(args, "--mcp-config", mcpConfigPath)
		if hasManagedMcpConfig(opts.McpConfig) {
			args = append(args, "--strict-mcp-config")
		}
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--effort", opts.ThinkingLevel)
	}
	blocked := claudeInteractiveBlockedArgs
	if opts.ClaudeSettingsPath != "" {
		blocked = make(map[string]blockedArgMode, len(claudeInteractiveBlockedArgs)+1)
		for key, mode := range claudeInteractiveBlockedArgs {
			blocked[key] = mode
		}
		blocked["--settings"] = blockedWithValue
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, blocked, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, blocked, logger)...)
	if opts.ClaudeSettingsPath != "" {
		args = append(args, "--settings", opts.ClaudeSettingsPath)
	}
	return args
}
```

- [ ] **Step 4: Run tests**

Run: `$GOTEST 'gofmt -l ./pkg/agent; go vet ./pkg/agent && go test ./pkg/agent -run "TestBuildClaude" -count=1 2>&1 | grep -E "^(--- FAIL|ok|FAIL)"'`
Expected: `ok` (the existing `TestBuildClaudeArgs*` tests keep passing).

- [ ] **Step 5: Commit**

```bash
git add server/pkg/agent/mcp_config.go server/pkg/agent/claude.go server/pkg/agent/claude_interactive_test.go
git commit -m "feat(agent): argument builder for interactive Claude Code sessions"
```

---

### Task 8: tmux runner — pure helpers, run script, state file, transcript tail

**Files:**
- Create: `server/internal/daemon/tmux_runner.go`
- Test: `server/internal/daemon/tmux_runner_test.go`

**Interfaces:**
- Produces (all in package daemon): `const tmuxTasksDirName = ".tmux-tasks"`, `const tmuxTranscriptTailLines = 200`, `var tmuxPollInterval = 2 * time.Second`; `func tmuxTaskDir(workspacesRoot, taskID string) string`; `func tmuxSessionName(issueIdentifier, taskID string, exists func(string) bool) string`; `func shellQuote(s string) string`; `func renderTmuxRunScript(folder, claudePath string, args []string, promptPath, exitCodePath string) string`; `type tmuxState struct{ Session, TaskID, IssueID, Folder, WorkDir, EnvRoot, TranscriptPath, ExitCodePath string; StartedAt time.Time }` (json tags in snake_case); `func writeTmuxState(dir string, st tmuxState) error`; `func readTmuxState(dir string) (tmuxState, error)`; `func readTmuxExitCode(path string) (code int, found bool, err error)`; `func transcriptTail(path string, lines int) string`.

- [ ] **Step 1: Write the failing tests**

```go
// server/internal/daemon/tmux_runner_test.go
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTmuxSessionNameIsSafeAndUnique(t *testing.T) {
	t.Parallel()
	taken := map[string]bool{"ctx-ctx-42-a1b2": true, "ctx-ctx-42-a1b2-2": true}
	exists := func(n string) bool { return taken[n] }
	if got := tmuxSessionName("CTX-42", "a1b2c3d4-0000", exists); got != "ctx-ctx-42-a1b2-3" {
		t.Fatalf("collision suffix: got %q", got)
	}
	if got := tmuxSessionName("Ops.Team:7", "zz99", func(string) bool { return false }); got != "ctx-ops-team-7-zz99" {
		t.Fatalf("sanitising: got %q", got)
	}
	if got := tmuxSessionName("", "abcd", func(string) bool { return false }); got != "ctx-task-abcd" {
		t.Fatalf("empty identifier fallback: got %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	if got := shellQuote("/Users/dev/it's here"); got != `'/Users/dev/it'\''s here'` {
		t.Fatalf("got %s", got)
	}
}

func TestRenderTmuxRunScriptLaunchesInteractiveClaudeAndRecordsExitCode(t *testing.T) {
	t.Parallel()
	script := renderTmuxRunScript("/Users/dev/app", "/usr/local/bin/claude", []string{"--model", "claude-opus-5"}, "/root/.tmux-tasks/t1/prompt.md", "/root/.tmux-tasks/t1/exit-code")
	for _, want := range []string{
		"#!/bin/sh",
		"cd '/Users/dev/app' ||",
		`'/usr/local/bin/claude' '--model' 'claude-opus-5' "$(cat '/root/.tmux-tasks/t1/prompt.md')"`,
		`code=$?`,
		`echo "$code" > '/root/.tmux-tasks/t1/exit-code'`,
		`exit "$code"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	for _, forbidden := range []string{" -p ", "--output-format", "bypassPermissions"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("script contains headless flag %q", forbidden)
		}
	}
}

func TestTmuxStateRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := tmuxState{Session: "ctx-ctx-1-abcd", TaskID: "abcd-1", IssueID: "issue-1", Folder: "/srv/app", WorkDir: "/srv/app", EnvRoot: "/tmp/env", TranscriptPath: filepath.Join(dir, "transcript.log"), ExitCodePath: filepath.Join(dir, "exit-code"), StartedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	if err := writeTmuxState(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := readTmuxState(dir)
	if err != nil || got != st {
		t.Fatalf("round trip: got %+v (%v), want %+v", got, err, st)
	}
	if _, err := readTmuxState(t.TempDir()); err == nil {
		t.Fatal("missing state must error")
	}
}

func TestReadTmuxExitCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, found, err := readTmuxExitCode(filepath.Join(dir, "none")); found || err != nil {
		t.Fatalf("missing file: found=%v err=%v", found, err)
	}
	p := filepath.Join(dir, "exit-code")
	os.WriteFile(p, []byte("0\n"), 0o600)
	if code, found, err := readTmuxExitCode(p); code != 0 || !found || err != nil {
		t.Fatalf("zero: %d %v %v", code, found, err)
	}
	os.WriteFile(p, []byte("137"), 0o600)
	if code, _, _ := readTmuxExitCode(p); code != 137 {
		t.Fatalf("non-zero: %d", code)
	}
	os.WriteFile(p, []byte("garbage"), 0o600)
	if _, _, err := readTmuxExitCode(p); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestTranscriptTailStripsAnsiAndKeepsLastLines(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "transcript.log")
	var b strings.Builder
	for i := 1; i <= 250; i++ {
		b.WriteString("\x1b[32mline ")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString("\x1b[0m\r\n")
	}
	b.WriteString("\x1b]0;title\x07final\n")
	os.WriteFile(p, []byte(b.String()), 0o600)
	tail := transcriptTail(p, 200)
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	if len(lines) != 200 {
		t.Fatalf("want 200 lines, got %d", len(lines))
	}
	if strings.Contains(tail, "\x1b") || strings.Contains(tail, "\r") {
		t.Fatalf("escape sequences survived: %q", tail[:80])
	}
	if lines[len(lines)-1] != "final" {
		t.Fatalf("last line = %q", lines[len(lines)-1])
	}
	if got := transcriptTail(filepath.Join(t.TempDir(), "missing"), 200); got != "" {
		t.Fatalf("missing transcript should give empty tail, got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `$GOTEST 'go test ./internal/daemon -run "TestTmux|TestShellQuote|TestRenderTmux|TestReadTmuxExitCode|TestTranscriptTail" -count=1'`
Expected: FAIL to compile (undefined helpers).

- [ ] **Step 3: Implement `tmux_runner.go` (helpers half)**

```go
package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// tmux mode (ContextPRO fork): a local_directory task runs as an interactive
// Claude Code session inside a tmux session in the user's folder. This file
// holds the pieces that need no tmux to test: names, the run script, the
// per-task state file and the transcript tail. See tmux_exec.go for the tmux
// commands and the bottom of this file for the runner and watch loop.

const (
	// tmuxTasksDirName lives directly under WorkspacesRoot. Dot-prefixed on
	// purpose: runGC skips every dot directory as daemon-internal.
	tmuxTasksDirName        = ".tmux-tasks"
	tmuxTranscriptTailLines = 200
)

// tmuxPollInterval is a var so tests can poll fast.
var tmuxPollInterval = 2 * time.Second

func tmuxTaskDir(workspacesRoot, taskID string) string {
	return filepath.Join(workspacesRoot, tmuxTasksDirName, taskID)
}

var tmuxNameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// tmuxSessionName builds `ctx-<issue identifier>-<task id prefix>`, lower-cased
// and reduced to [a-z0-9-] (tmux rejects "." and ":" in targets), with -2, -3,
// … appended while exists() reports a collision.
func tmuxSessionName(issueIdentifier, taskID string, exists func(string) bool) string {
	ident := strings.ToLower(strings.TrimSpace(issueIdentifier))
	if ident == "" {
		ident = "task"
	}
	short := taskID
	if len(short) > 4 {
		short = short[:4]
	}
	base := tmuxNameUnsafe.ReplaceAllString("ctx-"+ident+"-"+strings.ToLower(short), "-")
	base = strings.Trim(base, "-")
	name := base
	for n := 2; exists(name); n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

// shellQuote wraps s in single quotes for /bin/sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// renderTmuxRunScript is what the tmux session executes. It is a script rather
// than a tmux command string so quoting stays sane and the exit code can be
// captured: the session ends when the script ends, and the watch loop reads the
// exit-code file to decide complete vs fail. The prompt comes from a file so a
// multi-kilobyte task brief never has to survive tmux's command parsing.
func renderTmuxRunScript(folder, claudePath string, args []string, promptPath, exitCodePath string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, shellQuote(claudePath))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Generated by the ContextPRO daemon for one task. Safe to delete once the task ended.\n")
	b.WriteString("cd " + shellQuote(folder) + " || { echo 127 > " + shellQuote(exitCodePath) + "; exit 127; }\n")
	b.WriteString(strings.Join(quoted, " ") + ` "$(cat ` + shellQuote(promptPath) + `)"` + "\n")
	b.WriteString("code=$?\n")
	b.WriteString(`echo "$code" > ` + shellQuote(exitCodePath) + "\n")
	b.WriteString(`exit "$code"` + "\n")
	return b.String()
}

// tmuxState is written next to the run script so a restarted daemon can adopt
// the session (see adoptTmuxSessions).
type tmuxState struct {
	Session        string    `json:"session"`
	TaskID         string    `json:"task_id"`
	IssueID        string    `json:"issue_id"`
	Folder         string    `json:"folder"`
	WorkDir        string    `json:"work_dir"`
	EnvRoot        string    `json:"env_root"`
	TranscriptPath string    `json:"transcript_path"`
	ExitCodePath   string    `json:"exit_code_path"`
	StartedAt      time.Time `json:"started_at"`
}

const tmuxStateFile = "tmux.json"

func writeTmuxState(dir string, st tmuxState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, tmuxStateFile), data, 0o600)
}

func readTmuxState(dir string) (tmuxState, error) {
	var st tmuxState
	data, err := os.ReadFile(filepath.Join(dir, tmuxStateFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("tmux state %s: %w", dir, err)
	}
	return st, nil
}

// readTmuxExitCode returns (code, true, nil) when the run script recorded an
// exit code, (0, false, nil) when the file does not exist yet, and an error
// when the file exists but is not an integer.
func readTmuxExitCode(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false, fmt.Errorf("exit code file %s: %w", path, err)
	}
	return code, true, nil
}

// ansiEscapePattern covers CSI sequences (colours, cursor moves), OSC
// sequences (titles, hyperlinks) and character-set selects, plus carriage
// returns, which a terminal transcript is full of.
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\r`)

// transcriptTail returns the last `lines` lines of the pane transcript with
// terminal escapes stripped. A missing or unreadable transcript yields "".
func transcriptTail(path string, lines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	ring := make([]string, 0, lines)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := ansiEscapePattern.ReplaceAllString(scanner.Text(), "")
		if len(ring) == lines {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	return strings.Join(ring, "\n") + "\n"
}
```

- [ ] **Step 4: Run tests**

Run: `$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "TestTmux|TestShellQuote|TestRenderTmux|TestReadTmuxExitCode|TestTranscriptTail" -count=1 -v 2>&1 | grep -E "^(--- |ok|FAIL)"'`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/daemon/tmux_runner.go server/internal/daemon/tmux_runner_test.go
git commit -m "feat(daemon): tmux runner helpers, run script, state and transcript tail"
```

---

### Task 9: tmux controller

**Files:**
- Create: `server/internal/daemon/tmux_exec.go`
- Test: `server/internal/daemon/tmux_exec_test.go`

**Interfaces:**
- Consumes: `tmuxLookPath` (Task 1), `shellQuote` (Task 8).
- Produces: `type tmuxController interface { NewSession(ctx, name, folder string, command []string) error; PipePane(ctx, name, transcriptPath string) error; HasSession(ctx, name string) (bool, error); KillSession(ctx, name string) error }`; `type execTmux struct{ path string }` implementing it; `func newExecTmux() (*execTmux, error)`.

- [ ] **Step 1: Write the failing test with a fake tmux on PATH**

```go
// server/internal/daemon/tmux_exec_test.go
package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installFakeTmux writes a shell script named tmux that records every argv line
// to $log and keeps sessions as marker files in $sessions, so the controller
// can be exercised without a real tmux (repo rule: tests never resolve real
// agent or terminal binaries).
func installFakeTmux(t *testing.T) (binDir, log, sessions string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux is a sh script")
	}
	binDir = t.TempDir()
	log = filepath.Join(t.TempDir(), "argv.log")
	sessions = t.TempDir()
	script := `#!/bin/sh
echo "$@" >> "$FAKE_TMUX_LOG"
case "$1" in
  new-session) shift; while [ "$1" != "-s" ]; do shift; done; touch "$FAKE_TMUX_SESSIONS/$2"; exit 0 ;;
  has-session) name="${3#=}"; [ -f "$FAKE_TMUX_SESSIONS/$name" ] && exit 0 || exit 1 ;;
  kill-session) name="${3#=}"; rm -f "$FAKE_TMUX_SESSIONS/$name"; exit 0 ;;
  pipe-pane) exit 0 ;;
  *) echo "unexpected: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_LOG", log)
	t.Setenv("FAKE_TMUX_SESSIONS", sessions)
	t.Setenv("PATH", binDir)
	return binDir, log, sessions
}

func TestExecTmuxDrivesSessionsThroughTheBinary(t *testing.T) {
	_, log, _ := installFakeTmux(t)
	ctl, err := newExecTmux()
	if err != nil {
		t.Fatalf("newExecTmux: %v", err)
	}
	ctx := context.Background()

	if alive, err := ctl.HasSession(ctx, "ctx-x-1"); err != nil || alive {
		t.Fatalf("before new-session: alive=%v err=%v", alive, err)
	}
	if err := ctl.NewSession(ctx, "ctx-x-1", "/Users/dev/app", []string{"sh", "/tmp/run.sh"}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if alive, err := ctl.HasSession(ctx, "ctx-x-1"); err != nil || !alive {
		t.Fatalf("after new-session: alive=%v err=%v", alive, err)
	}
	if err := ctl.PipePane(ctx, "ctx-x-1", "/tmp/transcript.log"); err != nil {
		t.Fatalf("PipePane: %v", err)
	}
	if err := ctl.KillSession(ctx, "ctx-x-1"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if alive, _ := ctl.HasSession(ctx, "ctx-x-1"); alive {
		t.Fatal("session survived kill-session")
	}

	got, _ := os.ReadFile(log)
	for _, want := range []string{
		"new-session -d -s ctx-x-1 -c /Users/dev/app -- sh /tmp/run.sh",
		"has-session -t =ctx-x-1",
		"pipe-pane -o -t =ctx-x-1 cat >> '/tmp/transcript.log'",
		"kill-session -t =ctx-x-1",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("tmux was not invoked with %q; log:\n%s", want, got)
		}
	}
}

func TestNewExecTmuxFailsWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := newExecTmux(); err == nil {
		t.Fatal("expected an error when tmux is not on PATH")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `$GOTEST 'go test ./internal/daemon -run "TestExecTmux|TestNewExecTmux" -count=1'`
Expected: FAIL to compile: `undefined: newExecTmux`.

- [ ] **Step 3: Implement**

```go
// server/internal/daemon/tmux_exec.go
package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// tmuxController is the narrow slice of tmux the runner needs. An interface so
// the runner and adoption tests can use an in-memory fake; execTmux is the real
// thing, exercised in tmux_exec_test.go against a fake tmux script on PATH.
type tmuxController interface {
	NewSession(ctx context.Context, name, folder string, command []string) error
	PipePane(ctx context.Context, name, transcriptPath string) error
	HasSession(ctx context.Context, name string) (bool, error)
	KillSession(ctx context.Context, name string) error
}

type execTmux struct{ path string }

// newExecTmux resolves tmux through the same lookup the capability
// advertisement uses, so a daemon that advertised local-tmux-v1 can always
// build a controller.
func newExecTmux() (*execTmux, error) {
	path, err := tmuxLookPath()
	if err != nil {
		return nil, fmt.Errorf("tmux is not on this runtime's PATH: %w", err)
	}
	return &execTmux{path: path}, nil
}

func (t *execTmux) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, t.path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// NewSession starts a detached session whose only window runs command in
// folder. The session ends when command exits, which is what the watch loop
// relies on.
func (t *execTmux) NewSession(ctx context.Context, name, folder string, command []string) error {
	args := append([]string{"new-session", "-d", "-s", name, "-c", folder, "--"}, command...)
	return t.run(ctx, args...)
}

// PipePane tees everything the pane prints to transcriptPath. -o only starts a
// pipe when none is running, so re-running it is harmless.
func (t *execTmux) PipePane(ctx context.Context, name, transcriptPath string) error {
	return t.run(ctx, "pipe-pane", "-o", "-t", "="+name, "cat >> "+shellQuote(transcriptPath))
}

// HasSession uses the "=name" target form for an exact match; tmux otherwise
// treats the target as a prefix and "ctx-a-1" would match "ctx-a-10".
func (t *execTmux) HasSession(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, t.path, "has-session", "-t", "="+name)
	cmd.Stderr = nil
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session %s: %w", name, err)
}

func (t *execTmux) KillSession(ctx context.Context, name string) error {
	return t.run(ctx, "kill-session", "-t", "="+name)
}
```

- [ ] **Step 4: Run tests**

Run: `$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "TestExecTmux|TestNewExecTmux" -count=1 -v 2>&1 | grep -E "^(--- |ok|FAIL)"'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/internal/daemon/tmux_exec.go server/internal/daemon/tmux_exec_test.go
git commit -m "feat(daemon): tmux controller over the tmux binary"
```

---

### Task 10: The runner and the watch loop, wired into runTask

**Files:**
- Modify: `server/internal/daemon/tmux_runner.go` (append the runner half)
- Modify: `server/internal/daemon/daemon.go:560-600` (add `tmux tmuxController` field to `Daemon`), `:7700-7710` (branch before `taskLog.Debug("invoking backend", ...)`)
- Test: append to `server/internal/daemon/tmux_runner_test.go`

**Interfaces:**
- Consumes: `execenv.Environment{WorkDir, RootDir}`, `agent.ExecOptions`, `agent.BuildClaudeInteractiveArgs`, `agent.HasManagedMcpConfig` (Task 7), `tmuxController` (Task 9), `Client.ReportProgress(ctx, taskID, summary string, step, total int) error`, `Client.ReportTaskMessages(ctx, taskID string, []TaskMessageData) error`, `TaskResult{Status, Comment, WorkDir, EnvRoot}`, `d.rootCtx`, `d.cfg.WorkspacesRoot`, `d.cfg.DeviceName`.
- Produces: `func (d *Daemon) tmuxController() (tmuxController, error)`; `func (d *Daemon) runTmuxTask(ctx, task Task, env *execenv.Environment, assignment *localDirectoryAssignment, claudePath string, opts agent.ExecOptions, prompt string, taskLog *slog.Logger) (TaskResult, error)`; `func (d *Daemon) watchTmuxSession(ctx, ctl tmuxController, st tmuxState, taskLog *slog.Logger) (TaskResult, error)`; `func tmuxOutcome(st tmuxState) (TaskResult, error)`; `func (d *Daemon) announceTmuxSession(ctx, taskID, session string)`.

- [ ] **Step 1: Write the failing tests**

```go
// append to tmux_runner_test.go — Go allows only one import section before
// declarations, so MERGE these into the existing import block at the top.
import (
	"context"
	"log/slog"
	"sync"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// fakeTmux is an in-memory controller: sessions are alive until end() is
// called, and every call is recorded for assertions.
type fakeTmux struct {
	mu       sync.Mutex
	alive    map[string]bool
	newArgs  [][]string
	piped    map[string]string
	killed   []string
}

func newFakeTmux() *fakeTmux { return &fakeTmux{alive: map[string]bool{}, piped: map[string]string{}} }
func (f *fakeTmux) NewSession(_ context.Context, name, folder string, command []string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.alive[name] = true
	f.newArgs = append(f.newArgs, append([]string{name, folder}, command...))
	return nil
}
func (f *fakeTmux) PipePane(_ context.Context, name, transcript string) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.piped[name] = transcript; return nil
}
func (f *fakeTmux) HasSession(_ context.Context, name string) (bool, error) {
	f.mu.Lock(); defer f.mu.Unlock(); return f.alive[name], nil
}
func (f *fakeTmux) KillSession(_ context.Context, name string) error {
	f.mu.Lock(); defer f.mu.Unlock(); delete(f.alive, name); f.killed = append(f.killed, name); return nil
}
func (f *fakeTmux) end(name string) { f.mu.Lock(); defer f.mu.Unlock(); delete(f.alive, name) }

func newTmuxTestDaemon(t *testing.T, ctl tmuxController) *Daemon {
	t.Helper()
	d := &Daemon{cfg: Config{DaemonID: "d-tmux", WorkspacesRoot: t.TempDir(), DeviceName: "Mac mini (Test)"}, logger: slog.Default(), tmux: ctl}
	d.rootCtx = context.Background()
	return d
}

func TestRunTmuxTaskSpawnsSessionAndCompletesOnExitZero(t *testing.T) {
	orig := tmuxPollInterval
	tmuxPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tmuxPollInterval = orig })

	ctl := newFakeTmux()
	d := newTmuxTestDaemon(t, ctl)
	folder := t.TempDir()
	task := Task{ID: "abcd1234-task", IssueID: "issue-1", IssueIdentifier: "CTX-7"}
	assignment := &localDirectoryAssignment{Ref: localDirectoryRef{LocalPath: folder, DaemonID: "d-tmux", ExecutionMode: "tmux"}, AbsPath: folder, RealPath: folder}
	env := &execenv.Environment{WorkDir: folder, RootDir: filepath.Join(d.cfg.WorkspacesRoot, "env-1")}

	done := make(chan struct{})
	var result TaskResult
	var runErr error
	go func() {
		defer close(done)
		result, runErr = d.runTmuxTask(context.Background(), task, env, assignment, "/opt/fake/claude", agent.ExecOptions{Model: "claude-opus-5"}, "Do the thing", slog.Default())
	}()

	// Wait for the session to exist, then simulate Claude finishing.
	var name string
	for i := 0; i < 200 && name == ""; i++ {
		ctl.mu.Lock()
		for n := range ctl.alive {
			name = n
		}
		ctl.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if name != "ctx-ctx-7-abcd" {
		t.Fatalf("session name = %q", name)
	}
	taskDir := tmuxTaskDir(d.cfg.WorkspacesRoot, task.ID)
	if prompt, _ := os.ReadFile(filepath.Join(taskDir, "prompt.md")); string(prompt) != "Do the thing" {
		t.Fatalf("prompt file = %q", prompt)
	}
	script, _ := os.ReadFile(filepath.Join(taskDir, "run.sh"))
	if !strings.Contains(string(script), "'/opt/fake/claude' '--model' 'claude-opus-5'") {
		t.Fatalf("run.sh does not launch claude interactively:\n%s", script)
	}
	if ctl.piped[name] != filepath.Join(taskDir, "transcript.log") {
		t.Fatalf("pipe-pane transcript = %q", ctl.piped[name])
	}
	os.WriteFile(filepath.Join(taskDir, "transcript.log"), []byte("\x1b[1mhello\x1b[0m\nbye\n"), 0o600)
	os.WriteFile(filepath.Join(taskDir, "exit-code"), []byte("0\n"), 0o600)
	ctl.end(name)
	<-done

	if runErr != nil {
		t.Fatalf("runTmuxTask: %v", runErr)
	}
	if result.Status != "completed" || !strings.Contains(result.Comment, "hello\nbye") || result.WorkDir != folder {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatal("task dir was not cleaned up after completion")
	}
}

func TestWatchTmuxSessionMapsExitCodes(t *testing.T) {
	orig := tmuxPollInterval
	tmuxPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tmuxPollInterval = orig })
	ctl := newFakeTmux()
	d := newTmuxTestDaemon(t, ctl)

	mk := func(name string, exit string) tmuxState {
		dir := tmuxTaskDir(d.cfg.WorkspacesRoot, name)
		os.MkdirAll(dir, 0o700)
		if exit != "" {
			os.WriteFile(filepath.Join(dir, "exit-code"), []byte(exit), 0o600)
		}
		os.WriteFile(filepath.Join(dir, "transcript.log"), []byte("tail line\n"), 0o600)
		return tmuxState{Session: name, TaskID: name, TranscriptPath: filepath.Join(dir, "transcript.log"), ExitCodePath: filepath.Join(dir, "exit-code"), WorkDir: "/w", EnvRoot: "/e"}
	}
	if _, err := d.watchTmuxSession(context.Background(), ctl, mk("gone-nonzero", "2"), slog.Default()); err == nil || !strings.Contains(err.Error(), "exited with code 2") || !strings.Contains(err.Error(), "tail line") {
		t.Fatalf("non-zero exit: %v", err)
	}
	if _, err := d.watchTmuxSession(context.Background(), ctl, mk("gone-lost", ""), slog.Default()); err == nil || !strings.Contains(err.Error(), "session lost") {
		t.Fatalf("lost session: %v", err)
	}
	if res, err := d.watchTmuxSession(context.Background(), ctl, mk("gone-ok", "0"), slog.Default()); err != nil || res.Status != "completed" {
		t.Fatalf("clean exit: %+v %v", res, err)
	}
}

func TestWatchTmuxSessionKillsOnTaskCancelButNotOnDaemonShutdown(t *testing.T) {
	orig := tmuxPollInterval
	tmuxPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tmuxPollInterval = orig })

	// Task cancelled while the daemon keeps running: the session is killed.
	ctl := newFakeTmux()
	d := newTmuxTestDaemon(t, ctl)
	ctl.alive["ctx-a-1"] = true
	st := tmuxState{Session: "ctx-a-1", TaskID: "a", ExitCodePath: filepath.Join(t.TempDir(), "exit-code"), TranscriptPath: filepath.Join(t.TempDir(), "t.log")}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if _, err := d.watchTmuxSession(ctx, ctl, st, slog.Default()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(ctl.killed) != 1 || ctl.killed[0] != "ctx-a-1" {
		t.Fatalf("cancelled task should kill its session, killed=%v", ctl.killed)
	}

	// Daemon shutting down: the session must survive so it can be adopted.
	ctl2 := newFakeTmux()
	d2 := newTmuxTestDaemon(t, ctl2)
	rootCtx, rootCancel := context.WithCancel(context.Background())
	d2.rootCtx = rootCtx
	ctl2.alive["ctx-b-1"] = true
	st2 := tmuxState{Session: "ctx-b-1", TaskID: "b", ExitCodePath: filepath.Join(t.TempDir(), "exit-code"), TranscriptPath: filepath.Join(t.TempDir(), "t.log")}
	go func() { time.Sleep(30 * time.Millisecond); rootCancel() }()
	if _, err := d2.watchTmuxSession(rootCtx, ctl2, st2, slog.Default()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(ctl2.killed) != 0 || !ctl2.alive["ctx-b-1"] {
		t.Fatalf("daemon shutdown must leave the session alive, killed=%v alive=%v", ctl2.killed, ctl2.alive)
	}
}
```

Add `"errors"` to the test imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `$GOTEST 'go test ./internal/daemon -run "TestRunTmuxTask|TestWatchTmuxSession" -count=1'`
Expected: FAIL to compile: `d.tmux undefined`, `d.runTmuxTask undefined`.

- [ ] **Step 3: Add the controller field and accessor**

In `daemon.go`, inside `type Daemon struct` next to `localPathLocks *LocalPathLocker` add:
```go
	// tmux is the controller tmux-mode tasks use. nil means "build the real
	// one from PATH on first use"; tests inject a fake.
	tmux tmuxController
```

Append to `tmux_runner.go`:
```go
func (d *Daemon) tmuxController() (tmuxController, error) {
	if d.tmux != nil {
		return d.tmux, nil
	}
	ctl, err := newExecTmux()
	if err != nil {
		return nil, err
	}
	d.tmux = ctl
	return ctl, nil
}
```

- [ ] **Step 4: Implement the runner and watch loop**

Append to `tmux_runner.go` (add imports `"context"`, `"log/slog"`, `"github.com/multica-ai/multica/server/internal/daemon/execenv"`, `"github.com/multica-ai/multica/server/pkg/agent"`):

```go
// runTmuxTask is the tmux-mode counterpart of executeAndDrain. The folder has
// already been prepared by execenv.Prepare (brief, MCP config, sidecars), so
// this only has to launch the interactive session and wait for it to end.
func (d *Daemon) runTmuxTask(ctx context.Context, task Task, env *execenv.Environment, assignment *localDirectoryAssignment, claudePath string, opts agent.ExecOptions, prompt string, taskLog *slog.Logger) (TaskResult, error) {
	ctl, err := d.tmuxController()
	if err != nil {
		return TaskResult{}, err
	}
	dir := tmuxTaskDir(d.cfg.WorkspacesRoot, task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return TaskResult{}, fmt.Errorf("tmux task dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		cleanup()
		return TaskResult{}, fmt.Errorf("write tmux prompt: %w", err)
	}
	mcpPath := ""
	if agent.HasManagedMcpConfig(opts.McpConfig) {
		mcpPath = filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(mcpPath, opts.McpConfig, 0o600); err != nil {
			cleanup()
			return TaskResult{}, fmt.Errorf("write tmux mcp config: %w", err)
		}
	}
	exitPath := filepath.Join(dir, "exit-code")
	transcript := filepath.Join(dir, "transcript.log")
	args := agent.BuildClaudeInteractiveArgs(opts, mcpPath, d.logger)
	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte(renderTmuxRunScript(assignment.AbsPath, claudePath, args, promptPath, exitPath)), 0o700); err != nil {
		cleanup()
		return TaskResult{}, fmt.Errorf("write tmux run script: %w", err)
	}

	name := tmuxSessionName(task.IssueIdentifier, task.ID, func(n string) bool {
		alive, err := ctl.HasSession(ctx, n)
		return err == nil && alive
	})
	st := tmuxState{
		Session: name, TaskID: task.ID, IssueID: task.IssueID, Folder: assignment.AbsPath,
		WorkDir: env.WorkDir, EnvRoot: env.RootDir, TranscriptPath: transcript, ExitCodePath: exitPath,
		StartedAt: time.Now().UTC(),
	}
	if err := writeTmuxState(dir, st); err != nil {
		cleanup()
		return TaskResult{}, fmt.Errorf("write tmux state: %w", err)
	}
	if err := ctl.NewSession(ctx, name, assignment.AbsPath, []string{"sh", scriptPath}); err != nil {
		cleanup()
		return TaskResult{}, err
	}
	// Non-fatal: without the pipe the session still runs; only the final
	// transcript tail is lost, and the log says so.
	if err := ctl.PipePane(ctx, name, transcript); err != nil {
		taskLog.Warn("tmux: pipe-pane failed; run output will not be captured", "session", name, "error", err)
	}
	taskLog.Info("tmux: interactive session started", "session", name, "folder", assignment.AbsPath)
	d.announceTmuxSession(ctx, task.ID, name)
	return d.watchTmuxSession(ctx, ctl, st, taskLog)
}

// announceTmuxSession tells the issue where to attach. Nil client in tests.
func (d *Daemon) announceTmuxSession(ctx context.Context, taskID, session string) {
	if d.client == nil {
		return
	}
	text := fmt.Sprintf("Interactive session on %s: `tmux attach -t %s`", d.cfg.DeviceName, session)
	_ = d.client.ReportProgress(ctx, taskID, text, 1, 2)
	_ = d.client.ReportTaskMessages(ctx, taskID, []TaskMessageData{{Seq: 1, Type: "text", Content: text}})
}

// watchTmuxSession polls until the session is gone, then maps the recorded
// exit code to a result. On cancellation it distinguishes a cancelled TASK
// (kill the session) from a daemon shutdown (leave it; adoptTmuxSessions picks
// it up on the next start): if the daemon's root context is done, so is every
// task context, and killing then would destroy the user's live terminal.
func (d *Daemon) watchTmuxSession(ctx context.Context, ctl tmuxController, st tmuxState, taskLog *slog.Logger) (TaskResult, error) {
	ticker := time.NewTicker(tmuxPollInterval)
	defer ticker.Stop()
	for {
		alive, err := ctl.HasSession(ctx, st.Session)
		if err != nil {
			taskLog.Warn("tmux: has-session failed; retrying", "session", st.Session, "error", err)
		} else if !alive {
			return tmuxOutcome(st)
		}
		select {
		case <-ctx.Done():
			if d.rootCtx != nil && d.rootCtx.Err() != nil {
				taskLog.Info("tmux: daemon shutting down; leaving session for adoption", "session", st.Session)
			} else if killErr := ctl.KillSession(context.Background(), st.Session); killErr != nil {
				taskLog.Warn("tmux: kill-session after task cancel failed", "session", st.Session, "error", killErr)
			}
			return TaskResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// tmuxOutcome turns an ended session into the task's result and removes the
// task dir. The user's folder is never touched here.
func tmuxOutcome(st tmuxState) (TaskResult, error) {
	dir := filepath.Dir(st.ExitCodePath)
	defer os.RemoveAll(dir)
	tail := transcriptTail(st.TranscriptPath, tmuxTranscriptTailLines)
	code, found, err := readTmuxExitCode(st.ExitCodePath)
	switch {
	case err != nil:
		return TaskResult{}, fmt.Errorf("interactive session %s: %w", st.Session, err)
	case !found:
		return TaskResult{}, fmt.Errorf("interactive session %s ended without an exit code (session lost)\n\n%s", st.Session, tail)
	case code != 0:
		return TaskResult{}, fmt.Errorf("interactive session %s exited with code %d\n\n%s", st.Session, code, tail)
	}
	return TaskResult{
		Status:  "completed",
		Comment: fmt.Sprintf("Interactive session %s finished.\n\n%s", st.Session, tail),
		WorkDir: st.WorkDir,
		EnvRoot: st.EnvRoot,
	}, nil
}
```

- [ ] **Step 5: Wire it into runTask**

In `daemon.go`, immediately after `defer d.clearActiveRepoCheckoutTask(agentToken)` (about line 7707) and before `taskLog.Debug("invoking backend", ...)`:
```go
	// Fork (ContextPRO): tmux mode replaces the headless backend run with an
	// interactive session in the prepared folder. Everything above (folder
	// validation, execenv.Prepare, prompt, MCP config, model) is shared.
	if localAssignment != nil && localAssignment.UsesTmux() {
		return d.runTmuxTask(ctx, task, env, localAssignment, entry.Path, execOpts, prompt, taskLog)
	}
```
`localAssignment`, `entry`, `env`, `execOpts` and `prompt` are all in scope at that point (they are declared earlier in `runTask`; `entry.Path` is the value passed to `agent.ResolveBackend` as `ExecutablePath`).

- [ ] **Step 6: Run tests**

Run: `$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "Tmux|LocalDirectory" -count=1 -v 2>&1 | grep -E "^(--- |ok|FAIL)"'`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add server/internal/daemon/tmux_runner.go server/internal/daemon/tmux_runner_test.go server/internal/daemon/daemon.go
git commit -m "feat(daemon): run tmux-mode tasks as interactive Claude Code sessions"
```

---

### Task 11: Adopt live sessions after a daemon restart

**Files:**
- Modify: `server/internal/daemon/tmux_runner.go` (append)
- Modify: `server/internal/daemon/daemon.go:1962` (`Run`, after the `d.logger.Info("starting daemon", ...)` line)
- Test: append to `server/internal/daemon/tmux_runner_test.go`

**Interfaces:**
- Consumes: `d.reportTerminalTask(ctx, terminalTaskReport{kind, taskID, output, errorMessage, workDir}) error` with `terminalTaskReportComplete` / `terminalTaskReportFail`; `readTmuxState`, `watchTmuxSession`.
- Produces: `func (d *Daemon) adoptTmuxSessions(ctx context.Context)`; `func (d *Daemon) reportAdoptedTmuxOutcome(ctx, st tmuxState, result TaskResult, err error)`.

- [ ] **Step 1: Write the failing test**

```go
func TestAdoptTmuxSessionsResumesLiveAndSettlesDead(t *testing.T) {
	orig := tmuxPollInterval
	tmuxPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tmuxPollInterval = orig })

	ctl := newFakeTmux()
	d := newTmuxTestDaemon(t, ctl)
	var mu sync.Mutex
	settled := map[string]string{} // task id -> "completed" | "failed"
	d.tmuxAdoptionReport = func(st tmuxState, result TaskResult, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			settled[st.TaskID] = "failed: " + err.Error()
		} else {
			settled[st.TaskID] = result.Status
		}
	}

	mk := func(taskID, exit string, alive bool) {
		dir := tmuxTaskDir(d.cfg.WorkspacesRoot, taskID)
		os.MkdirAll(dir, 0o700)
		st := tmuxState{Session: "ctx-x-" + taskID, TaskID: taskID, TranscriptPath: filepath.Join(dir, "transcript.log"), ExitCodePath: filepath.Join(dir, "exit-code"), WorkDir: "/w"}
		writeTmuxState(dir, st)
		if exit != "" {
			os.WriteFile(st.ExitCodePath, []byte(exit), 0o600)
		}
		if alive {
			ctl.alive[st.Session] = true
		}
	}
	mk("live", "", true)
	mk("finished-ok", "0", false)
	mk("finished-bad", "1", false)
	mk("lost", "", false)
	os.MkdirAll(filepath.Join(d.cfg.WorkspacesRoot, tmuxTasksDirName, "corrupt"), 0o700)
	os.WriteFile(filepath.Join(d.cfg.WorkspacesRoot, tmuxTasksDirName, "corrupt", "tmux.json"), []byte("{not json"), 0o600)

	d.adoptTmuxSessions(context.Background())

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(settled)
		mu.Unlock()
		if n == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("dead sessions not settled in time: %v", settled)
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	if settled["finished-ok"] != "completed" || !strings.HasPrefix(settled["finished-bad"], "failed: interactive session") || !strings.Contains(settled["lost"], "session lost") {
		t.Fatalf("settled = %v", settled)
	}
	if _, ok := settled["live"]; ok {
		t.Fatal("live session must keep running, not settle")
	}
	mu.Unlock()

	// Ending the live session settles it too.
	os.WriteFile(filepath.Join(tmuxTaskDir(d.cfg.WorkspacesRoot, "live"), "exit-code"), []byte("0"), 0o600)
	ctl.end("ctx-x-live")
	deadline = time.After(2 * time.Second)
	for {
		mu.Lock()
		v := settled["live"]
		mu.Unlock()
		if v == "completed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("live session did not settle after it ended")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := os.Stat(filepath.Join(d.cfg.WorkspacesRoot, tmuxTasksDirName, "corrupt")); !os.IsNotExist(err) {
		t.Fatal("corrupt state dir should be removed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `$GOTEST 'go test ./internal/daemon -run TestAdoptTmuxSessions -count=1'`
Expected: FAIL to compile: `d.tmuxAdoptionReport undefined`, `d.adoptTmuxSessions undefined`.

- [ ] **Step 3: Implement**

Add to `type Daemon struct` next to `tmux tmuxController`:
```go
	// tmuxAdoptionReport is how adopted sessions report their outcome. nil
	// means "through the server client"; tests capture it instead.
	tmuxAdoptionReport func(st tmuxState, result TaskResult, err error)
```

Append to `tmux_runner.go`:
```go
// adoptTmuxSessions re-attaches to tmux-mode tasks that were running when the
// previous daemon process stopped. The tmux sessions themselves survive a
// daemon restart; only the watcher died. For each recorded task: a live session
// is watched again; a finished one is settled from its exit-code file; one with
// no session and no exit code is failed as lost. Runs once at startup.
func (d *Daemon) adoptTmuxSessions(ctx context.Context) {
	root := filepath.Join(d.cfg.WorkspacesRoot, tmuxTasksDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return // nothing recorded (or no root yet)
	}
	ctl, err := d.tmuxController()
	if err != nil {
		d.logger.Warn("tmux: cannot adopt sessions, tmux unavailable", "error", err, "pending", len(entries))
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		st, err := readTmuxState(dir)
		if err != nil {
			d.logger.Warn("tmux: dropping unreadable task state", "dir", dir, "error", err)
			_ = os.RemoveAll(dir)
			continue
		}
		go func(st tmuxState) {
			log := d.logger.With("task_id", st.TaskID, "tmux_session", st.Session)
			log.Info("tmux: adopting session from a previous daemon run")
			result, err := d.watchTmuxSession(ctx, ctl, st, log)
			if ctx.Err() != nil {
				return // shutting down again; the state file stays for the next start
			}
			d.reportAdoptedTmuxOutcome(ctx, st, result, err)
		}(st)
	}
}

func (d *Daemon) reportAdoptedTmuxOutcome(ctx context.Context, st tmuxState, result TaskResult, err error) {
	if d.tmuxAdoptionReport != nil {
		d.tmuxAdoptionReport(st, result, err)
		return
	}
	if d.client == nil {
		return
	}
	report := terminalTaskReport{kind: terminalTaskReportComplete, taskID: st.TaskID, output: result.Comment, workDir: st.WorkDir}
	if err != nil {
		report = terminalTaskReport{kind: terminalTaskReportFail, taskID: st.TaskID, errorMessage: err.Error(), workDir: st.WorkDir}
	}
	if rerr := d.reportTerminalTask(ctx, report); rerr != nil {
		// A task the server already closed (cancelled while we were down) lands
		// here; nothing more to do than say so.
		d.logger.Warn("tmux: reporting adopted session outcome failed", "task_id", st.TaskID, "error", rerr)
	}
}
```

In `daemon.go` `Run`, directly after the `d.logger.Info("starting daemon", logFields...)` line:
```go
	// Fork (ContextPRO): pick up interactive tmux sessions left by the
	// previous process before claiming new work.
	d.adoptTmuxSessions(ctx)
```

- [ ] **Step 4: Run tests**

Run: `$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "Tmux" -count=1 -v 2>&1 | grep -E "^(--- |ok|FAIL)"'`
Expected: all PASS.

- [ ] **Step 5: Full daemon and handler suites (non-root run for the daemon)**

Run the non-root recipe from "How to run Go checks" with `go test ./internal/daemon/... ./internal/handler/... ./pkg/agent -count=1`.
Expected: `ok` for every package.

- [ ] **Step 6: Commit**

```bash
git add server/internal/daemon/tmux_runner.go server/internal/daemon/tmux_runner_test.go server/internal/daemon/daemon.go
git commit -m "feat(daemon): adopt live tmux sessions after a restart"
```

---
### Task 12: Core types, schema, client parsing, and capability helper

**Files:**
- Modify: `packages/core/types/project.ts:88` (mode union)
- Modify: `packages/core/types/workspace.ts` (`Workspace` interface)
- Modify: `packages/core/api/schemas.ts` (add `LocalDirectoryRefSchema`, `parseDefaultLocalDirectory`)
- Modify: `packages/core/api/client.ts:2537-2560` (`listWorkspaces`, `getWorkspace`, `createWorkspace`, `updateWorkspace`)
- Modify: `packages/core/runtimes/cli-version.ts:150-180`
- Test: `packages/core/api/schema.test.ts` (append), `packages/core/runtimes/local-tmux-capability.test.ts` (new)

**Interfaces:**
- Consumes: `parseWithFallback(data: unknown, schema: ZodType, fallback: T, opts: { endpoint: string }): T` from `packages/core/api/schema.ts`.
- Produces: `LocalDirectoryExecutionMode = "in_place" | "worktree" | "tmux"`; `Workspace.default_local_directory: LocalDirectoryResourceRef | null`; `LocalDirectoryRefSchema`; `parseDefaultLocalDirectory(raw: unknown): LocalDirectoryResourceRef | null`; `api.updateWorkspace(id, { …, default_local_directory?: LocalDirectoryResourceRef | null })`; `LOCAL_TMUX_CAPABILITY = "local-tmux-v1"`, `runtimeAdvertisesCapability(runtimes, daemonId, capability)`, `runtimeAdvertisesLocalTmux(runtimes, daemonId)`.

- [ ] **Step 1: Write the failing tests**

Append to `packages/core/api/schema.test.ts`:

```ts
import { parseDefaultLocalDirectory } from "./schemas";

describe("workspace default_local_directory contract", () => {
  it("accepts every execution mode including tmux", () => {
    const ref = { local_path: "/Users/dev/app", daemon_id: "d-1", label: "App", execution_mode: "tmux" };
    expect(parseDefaultLocalDirectory(ref)).toEqual(ref);
  });
  it("falls back to null for absent, null, or malformed values", () => {
    expect(parseDefaultLocalDirectory(undefined)).toBeNull();
    expect(parseDefaultLocalDirectory(null)).toBeNull();
    // An old server or a corrupted row must never look like a configured folder.
    expect(parseDefaultLocalDirectory({ local_path: 5, daemon_id: "d" })).toBeNull();
    expect(parseDefaultLocalDirectory({ local_path: "/x", daemon_id: "d", execution_mode: "screen" })).toBeNull();
  });
});
```

New `packages/core/runtimes/local-tmux-capability.test.ts`:

```ts
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { LOCAL_TMUX_CAPABILITY, runtimeAdvertisesLocalTmux, runtimeAdvertisesLocalWorktree } from "./cli-version";

describe("runtimeAdvertisesLocalTmux", () => {
  const rows = [
    { daemon_id: "d-1", last_seen_at: "2026-09-01T00:00:00Z", metadata: { capabilities: ["local-worktree-v1"] } },
    { daemon_id: "d-1", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1", LOCAL_TMUX_CAPABILITY] } },
    { daemon_id: "d-2", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1"] } },
  ];
  it("reads the newest row of the daemon", () => {
    expect(runtimeAdvertisesLocalTmux(rows, "d-1")).toBe(true);
    expect(runtimeAdvertisesLocalTmux(rows, "d-2")).toBe(false);
    expect(runtimeAdvertisesLocalTmux(rows, null)).toBe(false);
  });
  it("keeps the worktree helper's answer", () => {
    expect(runtimeAdvertisesLocalWorktree(rows, "d-2")).toBe(true);
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd packages/core && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run api/schema.test.ts runtimes/local-tmux-capability.test.ts`
Expected: FAIL (`parseDefaultLocalDirectory` and `runtimeAdvertisesLocalTmux` are not exported).

- [ ] **Step 3: Types**

`packages/core/types/project.ts:88`:
```ts
export type LocalDirectoryExecutionMode = "in_place" | "worktree" | "tmux";
```

`packages/core/types/workspace.ts`: add `import type { LocalDirectoryResourceRef } from "./project";` and, in `Workspace` after `repos: WorkspaceRepo[];`:
```ts
  /**
   * Workspace-wide fallback folder for tasks (ContextPRO fork). Same ref shape
   * as a project's local_directory resource; null when unset. Projects with
   * their own local_directory resource ignore it.
   */
  default_local_directory: LocalDirectoryResourceRef | null;
```

- [ ] **Step 4: Schema and parser**

In `packages/core/api/schemas.ts` (imports: add `import { parseWithFallback } from "./schema";` and `import type { LocalDirectoryResourceRef } from "../types/project";` if not present):
```ts
export const LocalDirectoryRefSchema = z.object({
  local_path: z.string().min(1),
  daemon_id: z.string().min(1),
  label: z.string().optional(),
  execution_mode: z.enum(["in_place", "worktree", "tmux"]).optional(),
});

/**
 * workspace.default_local_directory as the UI may trust it. Older servers omit
 * the field and a corrupted row must not look like a configured folder, so
 * anything that is not a well-formed ref parses to null.
 */
export function parseDefaultLocalDirectory(raw: unknown): LocalDirectoryResourceRef | null {
  return parseWithFallback<LocalDirectoryResourceRef | null>(
    raw ?? null,
    LocalDirectoryRefSchema.nullable(),
    null,
    { endpoint: "workspace.default_local_directory" },
  );
}
```

- [ ] **Step 5: Client**

In `packages/core/api/client.ts`, add near the workspace methods:
```ts
// Every workspace read passes its default folder through the schema so UI code
// can rely on `default_local_directory` being a ref or null (never undefined).
function withParsedDefaultLocalDirectory(ws: Workspace): Workspace {
  return {
    ...ws,
    default_local_directory: parseDefaultLocalDirectory(
      (ws as { default_local_directory?: unknown }).default_local_directory,
    ),
  };
}
```
Then:
```ts
  async listWorkspaces(): Promise<Workspace[]> {
    const raw = await this.fetch<Workspace[]>("/api/workspaces");
    return raw.map(withParsedDefaultLocalDirectory);
  }
  async getWorkspace(id: string): Promise<Workspace> {
    return withParsedDefaultLocalDirectory(await this.fetch<Workspace>(`/api/workspaces/${id}`));
  }
```
`createWorkspace` wraps its result the same way. `updateWorkspace` gains `default_local_directory?: LocalDirectoryResourceRef | null` in its `data` type and wraps its result. Keep the existing request paths and bodies exactly as they are; only the return values change. Import `parseDefaultLocalDirectory` from `./schemas` and `LocalDirectoryResourceRef` from the types barrel.

- [ ] **Step 6: Capability helper**

In `packages/core/runtimes/cli-version.ts`, after `LOCAL_WORKTREE_CAPABILITY`:
```ts
export const LOCAL_TMUX_CAPABILITY = "local-tmux-v1";
```
Rename the body of `runtimeAdvertisesLocalWorktree` into a generic function and keep two thin wrappers:
```ts
export function runtimeAdvertisesCapability(
  runtimes: RuntimeCapabilityRow[],
  daemonId: string | null | undefined,
  capability: string,
): boolean {
  if (!daemonId) return false;
  let newest: RuntimeCapabilityRow | undefined;
  for (const rt of runtimes) {
    if (rt.daemon_id !== daemonId) continue;
    if (!newest) {
      newest = rt;
      continue;
    }
    // A row that never reported sorts oldest, so a live row always wins.
    const candidateSeen = rt.last_seen_at ?? "";
    const currentSeen = newest.last_seen_at ?? "";
    if (candidateSeen > currentSeen) newest = rt;
  }
  const metadata = newest?.metadata;
  if (!metadata || typeof metadata !== "object") return false;
  const caps = (metadata as { capabilities?: unknown }).capabilities;
  return Array.isArray(caps) && caps.includes(capability);
}

export function runtimeAdvertisesLocalWorktree(runtimes: RuntimeCapabilityRow[], daemonId: string | null | undefined): boolean {
  return runtimeAdvertisesCapability(runtimes, daemonId, LOCAL_WORKTREE_CAPABILITY);
}

export function runtimeAdvertisesLocalTmux(runtimes: RuntimeCapabilityRow[], daemonId: string | null | undefined): boolean {
  return runtimeAdvertisesCapability(runtimes, daemonId, LOCAL_TMUX_CAPABILITY);
}
```
Export the new names from the `@multica/core/runtimes` barrel next to `runtimeAdvertisesLocalWorktree` (`grep -rn runtimeAdvertisesLocalWorktree packages/core/runtimes/index.ts`).

- [ ] **Step 7: Run tests and typecheck**

Run: `cd packages/core && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run api/schema.test.ts runtimes/local-tmux-capability.test.ts && pnpm typecheck && cd ../.. && pnpm typecheck`
Expected: PASS; repo-wide typecheck clean (any `Workspace` literal in tests that lacks `default_local_directory` must be fixed by adding `default_local_directory: null`; `pnpm typecheck` lists them).

- [ ] **Step 8: Commit**

```bash
git add packages/core
git commit -m "feat(core): tmux execution mode type, workspace default folder schema, tmux capability helper"
```

---

### Task 13: Mode dialog card and project-resources wiring

**Files:**
- Modify: `packages/views/projects/components/local-directory-mode-dialog.tsx` (type, props, `LocalDirectoryModeOptions` — also export it)
- Modify: `packages/views/projects/components/project-resources-section.tsx:86-91` (`executionModeOf`), `:136-151` (capability lookups), `:526-545` (dialog props), `:556-563` (add `tmuxUnavailableReason`), `:689` (row label)
- Modify: `packages/views/locales/en/projects.json`, `zh-Hans/projects.json`, `ja/projects.json`, `ko/projects.json` (`resources` block)
- Test: append to `packages/views/projects/components/local-directory-mode-dialog.test.tsx`

**Interfaces:**
- Consumes: `LocalDirectoryExecutionMode` incl. `"tmux"` (Task 12); `runtimeAdvertisesLocalTmux` (Task 12); `useConfigStore((s) => s.localTmuxSupported)` (Task 3).
- Produces: `export type TmuxUnavailableReason = "runtime_no_tmux" | "server_outdated"`; dialog prop `tmuxUnavailableReason?: TmuxUnavailableReason`; `export function LocalDirectoryModeOptions(props: { value; onChange; unavailableReason?; tmuxUnavailableReason? })`.

- [ ] **Step 1: Write the failing tests**

Append to `local-directory-mode-dialog.test.tsx` (extend `renderDialog`'s overrides with `tmuxUnavailableReason?: TmuxUnavailableReason` and pass it through; import the type):

```ts
function tmuxOption(): HTMLElement {
  return screen.getAllByRole("radio")[2] as HTMLElement;
}

describe("LocalDirectoryModeDialog — interactive terminal (tmux)", () => {
  it("offers the tmux mode as a third choice and confirms it", () => {
    const { onConfirm } = renderDialog();
    expect(screen.getByText("Interactive terminal (tmux)")).toBeTruthy();
    fireEvent.click(tmuxOption());
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onConfirm).toHaveBeenCalledWith("tmux");
  });

  it("disables tmux with a reason when the runtime has no tmux", () => {
    renderDialog({ tmuxUnavailableReason: "runtime_no_tmux" });
    expect(tmuxOption().getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByText(/install tmux on that machine/i)).toBeTruthy();
  });

  it("disables tmux when the server predates the mode", () => {
    renderDialog({ tmuxUnavailableReason: "server_outdated" });
    expect(tmuxOption().getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByText(/server is too old/i)).toBeTruthy();
  });

  it("preselects tmux when editing a tmux resource", () => {
    renderDialog({ value: "tmux" });
    expect(tmuxOption().getAttribute("aria-checked")).toBe("true");
  });
});
```

Check how the existing worktree tests assert disabled state (`worktreeOption()` assertions a few lines above) and use the same attribute they use if it is not `aria-disabled`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd packages/views && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run projects/components/local-directory-mode-dialog.test.tsx`
Expected: FAIL: "Interactive terminal (tmux)" not found; `getAllByRole("radio")[2]` undefined.

- [ ] **Step 3: Locale keys (all four files, `resources` object)**

`en/projects.json`:
```json
"mode_tmux_title": "Interactive terminal (tmux)",
"mode_tmux_description": "Opens Claude Code in a tmux session inside this folder. You attach to watch and approve; tasks on this folder run side by side and each ends when its session ends.",
"mode_tmux_needs_tmux": "The ContextPRO runtime on that machine has no tmux. Install tmux on that machine and restart the runtime, then switch this on.",
"mode_tmux_needs_server_upgrade": "Your ContextPRO server is too old for interactive terminal mode. Update the server, then switch this on."
```
`zh-Hans/projects.json`:
```json
"mode_tmux_title": "交互式终端（tmux）",
"mode_tmux_description": "在此文件夹内的 tmux 会话中打开 Claude Code。你可以附加进去观看和批准；同一文件夹上的任务并行运行，各自在会话结束时结束。",
"mode_tmux_needs_tmux": "那台机器上的 ContextPRO 运行时没有 tmux。请在那台机器上安装 tmux 并重启运行时，然后再开启此模式。",
"mode_tmux_needs_server_upgrade": "你的 ContextPRO 服务器版本过旧，不支持交互式终端模式。请先升级服务器。"
```
`ja/projects.json`:
```json
"mode_tmux_title": "インタラクティブターミナル（tmux）",
"mode_tmux_description": "このフォルダ内の tmux セッションで Claude Code を開きます。アタッチして確認・承認でき、同じフォルダのタスクは並行して実行され、それぞれセッション終了時に完了します。",
"mode_tmux_needs_tmux": "そのマシンの ContextPRO ランタイムに tmux がありません。tmux をインストールしてランタイムを再起動してから有効にしてください。",
"mode_tmux_needs_server_upgrade": "ContextPRO サーバーが古いため、インタラクティブターミナルモードを使えません。サーバーを更新してから有効にしてください。"
```
`ko/projects.json`:
```json
"mode_tmux_title": "인터랙티브 터미널 (tmux)",
"mode_tmux_description": "이 폴더 안의 tmux 세션에서 Claude Code를 엽니다. 접속해서 지켜보고 승인할 수 있으며, 같은 폴더의 작업은 나란히 실행되고 각자 세션이 끝날 때 완료됩니다.",
"mode_tmux_needs_tmux": "해당 머신의 ContextPRO 런타임에 tmux가 없습니다. 그 머신에 tmux를 설치하고 런타임을 재시작한 뒤 켜세요.",
"mode_tmux_needs_server_upgrade": "ContextPRO 서버가 오래되어 인터랙티브 터미널 모드를 지원하지 않습니다. 서버를 업데이트한 뒤 켜세요."
```
Run `cd packages/views && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run locales` afterwards: the parity test must pass.

- [ ] **Step 4: Dialog**

In `local-directory-mode-dialog.tsx`:
- Import `Terminal` from `lucide-react` alongside `GitBranch, Pencil, TriangleAlert`.
- Add `export type TmuxUnavailableReason = "runtime_no_tmux" | "server_outdated";`
- Add `tmuxUnavailableReason?: TmuxUnavailableReason;` to `LocalDirectoryModeDialogProps` and to `LocalDirectoryModeOptionsProps`; pass it from the dialog to `<LocalDirectoryModeOptions … tmuxUnavailableReason={tmuxUnavailableReason} />`.
- Export the options component: `export function LocalDirectoryModeOptions(` (it is reused by the settings block in Task 14).
- After the worktree `<ModeOption>` add:
```tsx
      <ModeOption
        icon={<Terminal className="size-4" />}
        title={t(($) => $.resources.mode_tmux_title)}
        description={t(($) => $.resources.mode_tmux_description)}
        identifier="tmux"
        selected={value === "tmux"}
        disabled={tmuxUnavailableReason !== undefined}
        disabledReason={
          tmuxUnavailableReason === "runtime_no_tmux"
            ? t(($) => $.resources.mode_tmux_needs_tmux)
            : tmuxUnavailableReason === "server_outdated"
              ? t(($) => $.resources.mode_tmux_needs_server_upgrade)
              : undefined
        }
        onSelect={() => onChange("tmux")}
      />
```

- [ ] **Step 5: Resources section**

`executionModeOf`:
```ts
function executionModeOf(ref: LocalDirectoryResourceRef): LocalDirectoryExecutionMode {
  return ref.execution_mode === "worktree" || ref.execution_mode === "tmux"
    ? ref.execution_mode
    : "in_place";
}
```
Next to `serverValidatesWorktree` / `advertisesWorktree` add:
```ts
  const serverValidatesTmux = useConfigStore((state) => state.localTmuxSupported);
  const advertisesTmux = (daemonId: string | null) => runtimeAdvertisesLocalTmux(runtimes, daemonId);
```
(import `runtimeAdvertisesLocalTmux` from `@multica/core/runtimes`). Next to `worktreeUnavailableReason` add:
```ts
function tmuxUnavailableReason(
  runtimeHasTmux: boolean,
  serverValidates: boolean,
): TmuxUnavailableReason | undefined {
  if (!serverValidates) return "server_outdated";
  if (!runtimeHasTmux) return "runtime_no_tmux";
  return undefined;
}
```
and pass to the dialog:
```tsx
          tmuxUnavailableReason={tmuxUnavailableReason(
            advertisesTmux(modeDialog.daemonId),
            serverValidatesTmux,
          )}
```
At the row (line ~689, where `mode` drives a label or badge), make the tmux case render `t(($) => $.resources.mode_tmux_title)` wherever worktree renders `mode_worktree_title`. Read the few lines around 689 to place it; the pattern is a ternary or switch on `mode`.

- [ ] **Step 6: Run tests, typecheck**

Run: `cd packages/views && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run projects/components locales && pnpm typecheck`
Expected: PASS, including the pre-existing dialog and resources tests.

- [ ] **Step 7: Commit**

```bash
git add packages/views/projects packages/views/locales
git commit -m "feat(views): interactive terminal (tmux) mode in the local directory dialog"
```

---

### Task 14: Workspace default folder in Settings → Repositories

**Files:**
- Create: `packages/views/settings/components/default-local-directory-section.tsx`
- Modify: `packages/views/settings/components/repositories-tab.tsx:476` (render the section after the repositories `SettingsSection`)
- Modify: `packages/views/locales/{en,zh-Hans,ja,ko}/settings.json` (`repositories` object)
- Test: `packages/views/settings/components/default-local-directory-section.test.tsx`

**Interfaces:**
- Consumes: `api.updateWorkspace(id, { default_local_directory })` and `Workspace.default_local_directory` (Task 12); `LocalDirectoryModeOptions`, `TmuxUnavailableReason` (Task 13); `runtimeListOptions(wsId)` from `@multica/core/runtimes/queries` returning `RuntimeDevice[]` (`id`, `name`, `daemon_id`, `last_seen_at`, `metadata`); `useCurrentWorkspace()` from `@multica/core/paths`; `useWorkspaceId()` from `@multica/core/hooks`; `workspaceKeys.list()`; `SettingsSection`, `SettingsCard` from `./settings-layout`; `useConfigStore` from `@multica/core/config`; `runtimeAdvertisesLocalTmux`, `runtimeAdvertisesLocalWorktree` from `@multica/core/runtimes`.
- Produces: `export function DefaultLocalDirectorySection(): JSX.Element | null`.

- [ ] **Step 1: Write the failing test**

```tsx
// packages/views/settings/components/default-local-directory-section.test.tsx
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";
import enProjects from "../../locales/en/projects.json";

const mockUpdateWorkspace = vi.hoisted(() => vi.fn());
const workspaceRef = vi.hoisted(() => ({
  current: {
    id: "workspace-1",
    name: "Test Workspace",
    slug: "test-workspace",
    repos: [] as { url: string }[],
    default_local_directory: null as null | { local_path: string; daemon_id: string; execution_mode?: string; label?: string },
  },
}));
const runtimesRef = vi.hoisted(() => ({
  current: [
    { id: "rt-1", name: "Mac mini (Work)", daemon_id: "daemon-work", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1", "local-tmux-v1"] } },
    { id: "rt-2", name: "Laptop", daemon_id: "daemon-laptop", last_seen_at: "2026-09-02T00:00:00Z", metadata: { capabilities: ["local-worktree-v1"] } },
  ],
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: runtimesRef.current }),
  useQueryClient: () => ({ setQueryData: vi.fn() }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/paths", () => ({ useCurrentWorkspace: () => workspaceRef.current }));
vi.mock("@multica/core/workspace/queries", () => ({ workspaceKeys: { list: () => ["workspaces"] } }));
vi.mock("@multica/core/runtimes/queries", () => ({ runtimeListOptions: () => ({ queryKey: ["runtimes"] }) }));
vi.mock("@multica/core/api", () => ({ api: { updateWorkspace: mockUpdateWorkspace } }));
vi.mock("@multica/core/config", () => {
  const state = { localTmuxSupported: true, localWorktreeSupported: true };
  const useConfigStore = Object.assign((selector: (s: typeof state) => unknown) => selector(state), { getState: () => state });
  return { useConfigStore };
});
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { DefaultLocalDirectorySection } from "./default-local-directory-section";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings, projects: enProjects } };
function renderSection(): ReturnType<typeof render> {
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <DefaultLocalDirectorySection />
    </I18nProvider>,
  );
}

describe("DefaultLocalDirectorySection", () => {
  beforeEach(() => {
    mockUpdateWorkspace.mockReset();
    mockUpdateWorkspace.mockImplementation(async (_id: string, data: { default_local_directory: unknown }) => ({
      ...workspaceRef.current,
      default_local_directory: data.default_local_directory,
    }));
    workspaceRef.current.default_local_directory = null;
  });

  it("saves a default folder with the chosen runtime and mode", async () => {
    const user = userEvent.setup();
    renderSection();
    await user.selectOptions(screen.getByLabelText("Runtime"), "daemon-work");
    await user.type(screen.getByLabelText("Folder path"), "/Users/dev/contextpro");
    await user.click(screen.getByRole("radio", { name: /interactive terminal/i }));
    await user.click(screen.getByRole("button", { name: "Save default folder" }));
    await waitFor(() => expect(mockUpdateWorkspace).toHaveBeenCalledTimes(1));
    expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
      default_local_directory: { local_path: "/Users/dev/contextpro", daemon_id: "daemon-work", execution_mode: "tmux" },
    });
  });

  it("disables tmux for a runtime without the capability", async () => {
    const user = userEvent.setup();
    renderSection();
    await user.selectOptions(screen.getByLabelText("Runtime"), "daemon-laptop");
    expect(screen.getByRole("radio", { name: /interactive terminal/i }).getAttribute("aria-disabled")).toBe("true");
  });

  it("shows the saved default and clears it with null", async () => {
    workspaceRef.current.default_local_directory = { local_path: "/srv/app", daemon_id: "daemon-work", execution_mode: "in_place" };
    const user = userEvent.setup();
    renderSection();
    expect(screen.getByDisplayValue("/srv/app")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", { default_local_directory: null }));
  });

  it("keeps Save disabled until the path is absolute", async () => {
    const user = userEvent.setup();
    renderSection();
    await user.selectOptions(screen.getByLabelText("Runtime"), "daemon-work");
    await user.type(screen.getByLabelText("Folder path"), "relative/dir");
    expect(screen.getByRole("button", { name: "Save default folder" })).toBeDisabled();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd packages/views && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run settings/components/default-local-directory-section.test.tsx`
Expected: FAIL: cannot resolve `./default-local-directory-section`.

- [ ] **Step 3: Locale keys (`repositories` object in all four `settings.json`)**

`en`:
```json
"default_folder_title": "Default local folder",
"default_folder_description": "Tasks in projects without their own local folder run here. A project's own folder always wins.",
"default_folder_runtime": "Runtime",
"default_folder_runtime_placeholder": "Choose the machine",
"default_folder_path": "Folder path",
"default_folder_path_placeholder": "/Users/you/code/project",
"default_folder_path_hint": "Absolute path on the chosen machine.",
"default_folder_mode": "How tasks use this folder",
"default_folder_save": "Save default folder",
"default_folder_clear": "Clear",
"default_folder_saved": "Default folder saved",
"default_folder_cleared": "Default folder cleared",
"default_folder_save_failed": "Could not save the default folder",
"default_folder_none": "No default folder. Projects need their own local folder to run tasks on a machine."
```
`zh-Hans`:
```json
"default_folder_title": "默认本地文件夹",
"default_folder_description": "没有自己本地文件夹的项目，其任务在这里运行。项目自己的文件夹始终优先。",
"default_folder_runtime": "运行时",
"default_folder_runtime_placeholder": "选择机器",
"default_folder_path": "文件夹路径",
"default_folder_path_placeholder": "/Users/you/code/project",
"default_folder_path_hint": "所选机器上的绝对路径。",
"default_folder_mode": "任务如何使用此文件夹",
"default_folder_save": "保存默认文件夹",
"default_folder_clear": "清除",
"default_folder_saved": "默认文件夹已保存",
"default_folder_cleared": "默认文件夹已清除",
"default_folder_save_failed": "无法保存默认文件夹",
"default_folder_none": "尚无默认文件夹。项目需要自己的本地文件夹才能在机器上运行任务。"
```
`ja`:
```json
"default_folder_title": "デフォルトのローカルフォルダ",
"default_folder_description": "独自のローカルフォルダを持たないプロジェクトのタスクはここで実行されます。プロジェクト自身のフォルダが常に優先されます。",
"default_folder_runtime": "ランタイム",
"default_folder_runtime_placeholder": "マシンを選択",
"default_folder_path": "フォルダパス",
"default_folder_path_placeholder": "/Users/you/code/project",
"default_folder_path_hint": "選択したマシン上の絶対パス。",
"default_folder_mode": "タスクがこのフォルダを使う方法",
"default_folder_save": "デフォルトフォルダを保存",
"default_folder_clear": "クリア",
"default_folder_saved": "デフォルトフォルダを保存しました",
"default_folder_cleared": "デフォルトフォルダをクリアしました",
"default_folder_save_failed": "デフォルトフォルダを保存できませんでした",
"default_folder_none": "デフォルトフォルダはありません。マシン上でタスクを実行するには、プロジェクトに独自のローカルフォルダが必要です。"
```
`ko`:
```json
"default_folder_title": "기본 로컬 폴더",
"default_folder_description": "자체 로컬 폴더가 없는 프로젝트의 작업은 여기에서 실행됩니다. 프로젝트 자체 폴더가 항상 우선합니다.",
"default_folder_runtime": "런타임",
"default_folder_runtime_placeholder": "머신 선택",
"default_folder_path": "폴더 경로",
"default_folder_path_placeholder": "/Users/you/code/project",
"default_folder_path_hint": "선택한 머신의 절대 경로입니다.",
"default_folder_mode": "작업이 이 폴더를 사용하는 방식",
"default_folder_save": "기본 폴더 저장",
"default_folder_clear": "지우기",
"default_folder_saved": "기본 폴더를 저장했습니다",
"default_folder_cleared": "기본 폴더를 지웠습니다",
"default_folder_save_failed": "기본 폴더를 저장할 수 없습니다",
"default_folder_none": "기본 폴더가 없습니다. 머신에서 작업을 실행하려면 프로젝트에 자체 로컬 폴더가 필요합니다."
```

- [ ] **Step 4: Component**

```tsx
// packages/views/settings/components/default-local-directory-section.tsx
"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useConfigStore } from "@multica/core/config";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import { runtimeListOptions } from "@multica/core/runtimes/queries";
import { runtimeAdvertisesLocalTmux, runtimeAdvertisesLocalWorktree } from "@multica/core/runtimes";
import type { LocalDirectoryExecutionMode, LocalDirectoryResourceRef, Workspace } from "@multica/core/types";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { useT } from "../../i18n";
import {
  LocalDirectoryModeOptions,
  type TmuxUnavailableReason,
  type WorktreeUnavailableReason,
} from "../../projects/components/local-directory-mode-dialog";
import { SettingsCard, SettingsSection } from "./settings-layout";

/**
 * Workspace-wide fallback folder (ContextPRO fork). Saved through the normal
 * workspace update; the server validates the ref like a project resource and
 * gates worktree/tmux on the runtime's capability, so the picker only offers
 * what the chosen machine can run. Settings saves await the server (no
 * optimistic update): the value is a shared configuration, not a toggle.
 */
export function DefaultLocalDirectorySection() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const workspace = useCurrentWorkspace();
  const queryClient = useQueryClient();
  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId));
  const serverValidatesWorktree = useConfigStore((s) => s.localWorktreeSupported);
  const serverValidatesTmux = useConfigStore((s) => s.localTmuxSupported);

  const saved = workspace?.default_local_directory ?? null;
  const [daemonId, setDaemonId] = useState<string>(saved?.daemon_id ?? "");
  const [path, setPath] = useState<string>(saved?.local_path ?? "");
  const [mode, setMode] = useState<LocalDirectoryExecutionMode>(saved?.execution_mode ?? "in_place");
  const [saving, setSaving] = useState(false);

  // Re-seed the form when another client changes the saved value.
  useEffect(() => {
    setDaemonId(saved?.daemon_id ?? "");
    setPath(saved?.local_path ?? "");
    setMode(saved?.execution_mode ?? "in_place");
  }, [saved?.daemon_id, saved?.local_path, saved?.execution_mode]);

  const runtimeChoices = useMemo(
    () => runtimes.filter((rt) => typeof rt.daemon_id === "string" && rt.daemon_id !== ""),
    [runtimes],
  );
  const worktreeUnavailable: WorktreeUnavailableReason | undefined = !serverValidatesWorktree
    ? "server_outdated"
    : runtimeAdvertisesLocalWorktree(runtimes, daemonId || null)
      ? undefined
      : "not_git";
  const tmuxUnavailable: TmuxUnavailableReason | undefined = !serverValidatesTmux
    ? "server_outdated"
    : runtimeAdvertisesLocalTmux(runtimes, daemonId || null)
      ? undefined
      : "runtime_no_tmux";

  // Mirrors the server's isAbsoluteLocalPath: POSIX "/..." or Windows "C:\...".
  const pathIsAbsolute = /^\/|^[A-Za-z]:[\\/]/.test(path.trim());
  const canSave = !!workspace && daemonId !== "" && pathIsAbsolute && !saving;

  const applyUpdated = (updated: Workspace) => {
    queryClient.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
      old?.map((item) => (item.id === updated.id ? updated : item)),
    );
  };

  const save = async () => {
    if (!workspace || !canSave) return;
    const ref: LocalDirectoryResourceRef = { local_path: path.trim(), daemon_id: daemonId, execution_mode: mode };
    setSaving(true);
    try {
      applyUpdated(await api.updateWorkspace(workspace.id, { default_local_directory: ref }));
      toast.success(t(($) => $.repositories.default_folder_saved));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.repositories.default_folder_save_failed));
    } finally {
      setSaving(false);
    }
  };

  const clear = async () => {
    if (!workspace) return;
    setSaving(true);
    try {
      applyUpdated(await api.updateWorkspace(workspace.id, { default_local_directory: null }));
      toast.success(t(($) => $.repositories.default_folder_cleared));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.repositories.default_folder_save_failed));
    } finally {
      setSaving(false);
    }
  };

  if (!workspace) return null;

  return (
    <SettingsSection
      title={t(($) => $.repositories.default_folder_title)}
      description={t(($) => $.repositories.default_folder_description)}
    >
      <SettingsCard>
        <div className="flex flex-col gap-4 p-4">
          {saved === null && (
            <p className="text-caption text-muted-foreground">{t(($) => $.repositories.default_folder_none)}</p>
          )}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="default-folder-runtime">{t(($) => $.repositories.default_folder_runtime)}</Label>
            {/* Native select: a handful of machines, and it keeps the test and
                keyboard story simple. */}
            <select
              id="default-folder-runtime"
              className="h-9 rounded-md border border-input bg-background px-3 text-body"
              value={daemonId}
              onChange={(e) => setDaemonId(e.target.value)}
            >
              <option value="">{t(($) => $.repositories.default_folder_runtime_placeholder)}</option>
              {runtimeChoices.map((rt) => (
                <option key={rt.id} value={rt.daemon_id ?? ""}>
                  {rt.name}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="default-folder-path">{t(($) => $.repositories.default_folder_path)}</Label>
            <Input
              id="default-folder-path"
              value={path}
              placeholder={t(($) => $.repositories.default_folder_path_placeholder)}
              onChange={(e) => setPath(e.target.value)}
            />
            <p className="text-micro text-muted-foreground">{t(($) => $.repositories.default_folder_path_hint)}</p>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>{t(($) => $.repositories.default_folder_mode)}</Label>
            <LocalDirectoryModeOptions
              value={mode}
              onChange={setMode}
              unavailableReason={worktreeUnavailable}
              tmuxUnavailableReason={tmuxUnavailable}
            />
          </div>
          <div className="flex items-center gap-2">
            <Button onClick={() => void save()} disabled={!canSave}>
              {t(($) => $.repositories.default_folder_save)}
            </Button>
            <Button variant="outline" onClick={() => void clear()} disabled={saving || saved === null}>
              {t(($) => $.repositories.default_folder_clear)}
            </Button>
          </div>
        </div>
      </SettingsCard>
    </SettingsSection>
  );
}
```

Notes for the implementer: `LocalDirectoryModeOptions` uses the `projects` i18n namespace internally, so the settings page must have that namespace loaded (the web and desktop apps load all namespaces; the test passes `enProjects`). If `SettingsSection` does not accept a `title` prop, look at `settings-layout.tsx` and use whichever prop renders the heading. The worktree card in this picker uses `"not_git"` as the "runtime cannot" reason because the dialog has no dedicated runtime-capability reason for worktree; the rendered text still tells the user to pick another mode.

- [ ] **Step 5: Render it in the tab**

In `repositories-tab.tsx`, right after the closing `</SettingsSection>` of the repositories section (line ~476) add `<DefaultLocalDirectorySection />` and import it from `./default-local-directory-section`.

- [ ] **Step 6: Run tests, typecheck, lint**

Run: `cd packages/views && NODE_OPTIONS=--no-experimental-webstorage pnpm exec vitest run settings/components locales && pnpm typecheck && cd ../.. && pnpm lint`
Expected: PASS; no lint errors.

- [ ] **Step 7: Commit**

```bash
git add packages/views/settings packages/views/locales
git commit -m "feat(views): workspace default local folder in Settings"
```

---

### Task 15: Built-in skill docs and spec alignment

**Files:**
- Modify: `server/internal/service/builtin_skills/multica-runtimes-and-repos/SKILL.md` (after the paragraph that starts "`runtime update` and `runtime delete` are writes." — line 48)
- Modify: `server/internal/service/builtin_skills/multica-runtimes-and-repos/references/runtimes-and-repos-source-map.md` (append two bullets)
- Modify: `docs/superpowers/specs/2026-09-02-tmux-execution-mode-design.md` (the "non-zero fails with the same tail and `failure_reason = "interactive_session_exit"`" sentence)

- [ ] **Step 1: SKILL.md paragraph**

Insert after line 48:
```markdown
A project's local directory can run tasks in one of three execution modes. `in_place` edits the folder directly, one task at a time. `worktree` gives each task its own git worktree and hands back a branch. `tmux` (ContextPRO only) opens an interactive Claude Code session in a tmux session inside the folder; the task's first progress message names the session (`tmux attach -t ctx-<issue>-<id>` on that machine), a human may watch, answer permission prompts and steer, and the task completes when the session ends (exit code 0) or fails otherwise. A workspace-wide default folder applies to projects without their own local directory. Nothing about issue status commands changes for a tmux task.
```

- [ ] **Step 2: Source map bullets**

Append:
```markdown
- `server/internal/daemon/tmux_runner.go` and `tmux_exec.go` implement `execution_mode=tmux` for local directories: the daemon writes a per-task prompt and run script under `<workspaces root>/.tmux-tasks/<task id>/`, starts `tmux new-session` in the folder running interactive `claude`, tees the pane with `pipe-pane`, polls `has-session` and reports completion from the recorded exit code. Sessions survive a daemon restart and are re-adopted in `adoptTmuxSessions`.
- `server/internal/handler/project_resource.go` validates the mode, gates it on the `local-tmux-v1` daemon capability (`requireModeCapableDaemon`), and synthesizes `workspace.default_local_directory` into a claim's resources when the project has no local directory (`resolveClaimProjectContext`).
```

- [ ] **Step 3: Spec alignment**

In the spec, replace `non-zero fails with the same tail and \`failure_reason = "interactive_session_exit"\`` with `non-zero fails with the same tail; the failure reason stays the daemon's default classification, since the report path has no per-mode reason and adding one bought nothing visible`. Keep the rest of the sentence.

Also in the spec's Daemon section, change "only when `exec.LookPath(\"tmux\")` succeeds at startup. The lookup result is cached on the daemon and logged once at startup" to "only when `tmux` resolves on PATH; the lookup runs on every capability advertisement (one PATH scan) so installing tmux and restarting the daemon is enough". The plan implements the re-check, not a cached startup probe.

- [ ] **Step 4: Commit**

```bash
git add server/internal/service/builtin_skills/multica-runtimes-and-repos docs/superpowers/specs/2026-09-02-tmux-execution-mode-design.md
git commit -m "docs(skills): describe the tmux execution mode and the workspace default folder"
```

---

### Task 16: Deploy — backend, web, and the fork's daemon on the Mac Mini

**Files:** none in the repo. Operations only. Run every command from `/Users/bogdand/Gits/bvdr/multica`.

**Interfaces:**
- Consumes: the built images from the compose build override; the launchd plists `~/Library/LaunchAgents/com.multica.daemon.personal.plist` and `com.multica.daemon.work.plist` (ProgramArguments[0] is `/opt/homebrew/bin/multica`).

- [ ] **Step 1: Final verification before deploying**

```bash
pnpm typecheck && pnpm lint && NODE_OPTIONS=--no-experimental-webstorage pnpm test
# Go, non-root recipe from "How to run Go checks":
#   go test ./internal/... ./cmd/... ./pkg/agent -count=1
git status --porcelain   # must be empty; everything committed
git push origin custom
```
Expected: all green except the known local-only failures documented in memory (desktop Electron binary file). Anything else stops the deploy.

- [ ] **Step 2: Build and swap backend + web**

```bash
export VERSION="$(git describe --tags --always)" COMMIT="$(git rev-parse --short HEAD)" DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml build
docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml up -d
bash scripts/selfhost-wait.sh build
curl -s http://127.0.0.1:8480/health            # commit must equal $COMMIT
curl -s http://127.0.0.1:3400/api/config | grep -o '"local_tmux_supported":true'
```

- [ ] **Step 3: Cross-compile the fork CLI for the Mac Mini**

```bash
docker run --rm -v "$PWD":/repo -w /repo/server -v multica-gomod:/go/pkg/mod -v multica-gocache:/root/.cache/go-build \
  -e GOOS=darwin -e GOARCH=arm64 -e CGO_ENABLED=0 golang:1.26-alpine \
  sh -c "go build -ldflags '-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE' -o /repo/dist/contextpro-multica ./cmd/multica"
mkdir -p ~/.local/bin && install -m 0755 dist/contextpro-multica ~/.local/bin/contextpro-multica
~/.local/bin/contextpro-multica --version     # prints the fork version, not 0.4.37
```
`dist/` is gitignored; if it is not, do not commit it.

- [ ] **Step 4: Point both launchd daemons at the fork binary**

```bash
for job in com.multica.daemon.personal com.multica.daemon.work; do
  P=~/Library/LaunchAgents/$job.plist
  cp "$P" "$P.bak-$(date +%Y%m%d%H%M%S)"                       # keep a rollback copy
  plutil -replace ProgramArguments.0 -string "$HOME/.local/bin/contextpro-multica" "$P"
  launchctl bootout "gui/$(id -u)/$job" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$P"
done
sleep 5; launchctl list | grep multica.daemon                  # both jobs, exit status 0
```
The plists' PATH already contains `/opt/homebrew/bin`, where tmux lives, so the daemon finds tmux and advertises `local-tmux-v1`.

- [ ] **Step 5: Verify the capability reached the server**

In ContextPRO → Settings → Repositories, the runtime picker in "Default local folder" offers "Interactive terminal (tmux)" enabled for both Mac Mini runtimes. Or via API: `curl -s -H "Cookie: $SESSION" http://127.0.0.1:3400/api/runtimes | grep -o 'local-tmux-v1' | head -1`.

- [ ] **Step 6: End-to-end smoke**

1. Set the workspace default folder to an existing repo on the Mac Mini with mode "Interactive terminal (tmux)".
2. Create an issue in a project without its own folder and assign it to an agent bound to that runtime.
3. Within a few seconds `tmux ls` on the Mac Mini shows `ctx-<issue>-<id>` and the issue's run shows the attach command.
4. `tmux attach -t <name>`: Claude Code is interactive, the task brief is its first message, permission prompts appear (and reach Moshi).
5. Quit Claude Code: the issue's task completes and its output shows the transcript tail.
6. Rollback if needed: restore the `.bak-*` plist and `launchctl bootout` / `bootstrap` again; the previous images are still tagged locally.

- [ ] **Step 7: Record the outcome**

Update the deployment memory note (`macmini-multica-deployment`) with the daemon binary path, the plist change, and the rollback copy names.
