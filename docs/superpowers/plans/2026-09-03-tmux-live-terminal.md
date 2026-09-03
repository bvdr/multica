# tmux Live Terminal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show a real xterm.js terminal on the issue page attached to the running tmux task's session, let any member with issue access watch and type, tie the session's life to the task (Claude exit, session close, or the card moving to done/cancelled all complete it), and store the Claude Code session id on the task so it can be resumed with its context.

**Architecture:** One tmux client per viewer. The browser opens a task-scoped WebSocket to the server; the server sends the daemon a `daemon:terminal_open` hint over the existing daemon socket; the daemon starts `tmux attach-session` inside a pseudo-terminal (creack/pty) and dials back a second WebSocket; an in-memory bridge (`server/internal/terminalbridge`) copies binary frames between the pair and relays JSON control frames (resize, exit, status, viewers). The daemon reports the live session name to a new nullable `agent_task_queue.tmux_session` column, the issue status change and a Close-session endpoint complete tmux tasks server-side, and Claude Code is launched with `--session-id` (or `--resume <prior>`) so the id is deterministic.

**Tech Stack:** Go 1.26 (gorilla/websocket 1.5.3, creack/pty v1.1.24, sqlc, PostgreSQL migrations; tests run in the `golang:1.26-alpine` container), TypeScript/React 19 with TanStack Query, zod, `@xterm/xterm` 6.0.0 + `@xterm/addon-fit` 0.11.0, vitest (node env for pure tests, jsdom for components).

**Spec:** `docs/superpowers/specs/2026-09-03-tmux-live-terminal-design.md` (read it first; this plan argues from it). The previous plan `docs/superpowers/plans/2026-09-02-tmux-execution-mode.md` explains the tmux mode this builds on.

## Global Constraints

- Event name is exactly `daemon:terminal_open`; capability string is exactly `local-tmux-terminal-v1`; the column is exactly `agent_task_queue.tmux_session` (TEXT, nullable, no index); migration number is `445`.
- Browser endpoint is `GET /api/tasks/{taskID}/terminal/ws`; daemon endpoint is `GET /api/daemon/terminals/{terminalID}/ws`; daemon report is `POST /api/daemon/tasks/{taskID}/tmux`; member action is `POST /api/tasks/{taskID}/tmux/close`.
- Control frames are single JSON objects in text frames: `resize`, `exit`, `status`, `viewers`, `auth`, `auth_ack`. Binary frames are raw bytes. Output is chunked at 32 KB. `status.reason` is one of `session_ended`, `task_not_running`, `runtime_offline`, `runtime_no_terminal`, `runtime_unreachable`, `too_many_terminals`, `viewer_too_slow`, `closed`.
- Limits: 4 terminals per task, 12 per runtime (server) and 12 attaches per daemon; viewer outbound queue 256 frames; daemon dial-back timeout 10 s; socket write deadline 10 s.
- Anyone who is a member of the task's workspace may view and type. No other permission check.
- Interactive launch args stay `--allowedTools Bash(multica:*) --permission-mode manual`, plus exactly one of `--session-id <uuid>` (fresh) or `--resume <prior_session_id>`. The blocked custom-arg list is unchanged.
- Missing exit code at session end is now a **completed** outcome with comment `Session closed before Claude Code exited.`; non-zero exit stays failed; exit 0 stays completed.
- Issue status changes complete/cancel only tasks whose `tmux_session` is set. Headless tasks keep upstream behaviour (MUL-4113).
- No foreign keys, no cascades, no non-concurrent index in migrations (this change adds no index).
- Default tests never resolve real `tmux` or `claude`; they install fake executables in a temp dir first on `PATH`. PTY tests run as the non-root user.
- `packages/core` gets no `react-dom`, `localStorage`, `process.env`, or UI imports; `packages/views` gets no `next/*` or router imports. New TS deps go through `catalog:`.
- Copy says ContextPRO. English comments. Add a comment on every non-obvious decision.
- No Go toolchain on the Mac Mini: run every Go command through the container recipe below. Frontend tests need `NODE_OPTIONS=--no-experimental-webstorage`.
- Commit after each task with a conventional prefix (`feat(daemon)`, `feat(server)`, `feat(views)`, `docs`, …). Never commit `.env`. No AI co-author trailers.

## How to run Go checks

```bash
cd /Users/bogdand/Gits/bvdr/multica
docker network create multica-test-go >/dev/null 2>&1 || true
docker rm -f multica-test-go-pg >/dev/null 2>&1 || true
docker run -d --name multica-test-go-pg --network multica-test-go \
  -e POSTGRES_USER=multica -e POSTGRES_PASSWORD=multica -e POSTGRES_DB=multica pgvector/pgvector:pg17 >/dev/null && sleep 4
# GOTEST runs a shell snippet inside the golang container, repo mounted at /repo, cwd /repo/server.
GOTEST='docker run --rm --network multica-test-go -e DATABASE_URL=postgres://multica:multica@multica-test-go-pg:5432/multica?sslmode=disable -e APP_ENV= -e JWT_SECRET=test-only-secret -v "$PWD":/repo -w /repo/server -v multica-gomod:/go/pkg/mod -v multica-gocache:/root/.cache/go-build golang:1.26-alpine sh -c'
$GOTEST 'go run ./cmd/migrate up >/dev/null; echo migrated'
# Example: $GOTEST 'gofmt -l ./internal/terminalbridge; go vet ./internal/terminalbridge && go test ./internal/terminalbridge -count=1'
# Non-root variant (needed for PTY / process-spawning daemon tests — Claude Code refuses root and PTY tests need a real tty user):
GOTEST_NR='docker run --rm --network multica-test-go -e DATABASE_URL=postgres://multica:multica@multica-test-go-pg:5432/multica?sslmode=disable -e APP_ENV= -e JWT_SECRET=test-only-secret -v "$PWD":/repo -w /repo/server -v multica-gomod:/go/pkg/mod -v multica-gocache:/root/.cache/go-build golang:1.26-alpine sh -c'
# Body for GOTEST_NR (single-quoted argument): apk add --no-cache git bash >/dev/null 2>&1; adduser -D tester 2>/dev/null; chown -R tester /go/pkg/mod; mkdir -p /tmp/gocache /tmp/home && chown tester /tmp/gocache /tmp/home; su tester -c "cd /repo/server && HOME=/tmp/home GOCACHE=/tmp/gocache APP_ENV= JWT_SECRET=test-only-secret DATABASE_URL=postgres://multica:multica@multica-test-go-pg:5432/multica?sslmode=disable go test ./internal/daemon -run TestTmux -count=1"
# sqlc regeneration (after editing server/pkg/db/queries/*.sql or a migration): make sqlc  — or in the container: $GOTEST 'go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate'
# Teardown at the very end: docker rm -f multica-test-go-pg; docker network rm multica-test-go
```

## How to run TS checks

```bash
cd /Users/bogdand/Gits/bvdr/multica
export NODE_OPTIONS=--no-experimental-webstorage
pnpm --filter @multica/core exec vitest run <path-relative-to-packages/core>
pnpm --filter @multica/views exec vitest run <path-relative-to-packages/views>
pnpm --filter @multica/core typecheck && pnpm --filter @multica/views typecheck
pnpm lint
```

---

## File structure

New files and their single responsibility:

| File | Responsibility |
| --- | --- |
| `server/pkg/protocol` (events.go, messages.go) | the `daemon:terminal_open` event, its payload, the `local-tmux-terminal-v1` capability |
| `server/internal/daemon/terminal.go` | daemon side: verify the hint against its own tmux state, attach a tmux client in a PTY, dial back, pump bytes |
| `server/internal/daemon/tmux_exec.go` | `tmuxController.Attach` (PTY around `tmux attach-session`) and `window-size latest` |
| `server/internal/daemon/tmux_runner.go` | fixed Claude session id, resume, session report, "session closed" = completed |
| `server/internal/daemonws/hub.go` | `NotifyTerminalOpen`: hand the hint to one capable daemon connection |
| `server/internal/terminalbridge/bridge.go` | pair one viewer socket with one daemon socket, relay control frames, limits, viewer counts |
| `server/internal/realtime/hub.go` | exported `AuthenticateUpgrade` (cookie or first-frame token) for the viewer endpoint |
| `server/internal/handler/tmux_task.go` | daemon report endpoint, Close-session endpoint, issue-status hook |
| `server/internal/handler/terminal.go` | the two WebSocket endpoints (viewer, daemon dial-back) |
| `server/migrations/445_*` + `server/pkg/db/queries/agent.sql` | `agent_task_queue.tmux_session` and its setter |
| `packages/core/api/terminal-client.ts` | browser/desktop WebSocket client with auth, control frames, reconnect policy |
| `packages/views/issues/components/tmux-terminal-section.tsx` | the xterm.js panel in the issue main column, Close session, expand |
| `packages/views/issues/components/execution-log-section.tsx` | "Terminal" chip on active tmux rows, resume affordance on completed ones |

Ordering: Tasks 1–5 (daemon side) and 6–10 (server side) are independent of each other except that both need Task 1. Tasks 11–13 (web) need Task 7's response field. Task 15 needs everything.

---

### Task 1: Protocol — event, payload, capability

**Files:**
- Modify: `server/pkg/protocol/events.go` (after `EventDaemonPendingWork`, line ~160)
- Modify: `server/pkg/protocol/messages.go` (after `DaemonCapabilityLocalTmuxV1`, line ~33; after `PendingWorkPayload`, line ~145)
- Test: `server/pkg/protocol/terminal_test.go`

**Interfaces:**
- Produces: `EventDaemonTerminalOpen = "daemon:terminal_open"`, `DaemonCapabilityLocalTmuxTerminalV1 = "local-tmux-terminal-v1"`, `TerminalOpenPayload{TerminalID, TaskID, Session string; Cols, Rows int}` with JSON keys `terminal_id`, `task_id`, `session`, `cols`, `rows`.

- [ ] **Step 1: Write the failing test**

```go
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// The daemon and the server share these names; a typo on either side would
// silently make every terminal open time out as "runtime_unreachable".
func TestTerminalOpenFrameRoundTrip(t *testing.T) {
	if EventDaemonTerminalOpen != "daemon:terminal_open" {
		t.Fatalf("event name = %q", EventDaemonTerminalOpen)
	}
	if DaemonCapabilityLocalTmuxTerminalV1 != "local-tmux-terminal-v1" {
		t.Fatalf("capability = %q", DaemonCapabilityLocalTmuxTerminalV1)
	}
	in := TerminalOpenPayload{TerminalID: "t-1", TaskID: "task-1", Session: "ctx-foli-39-01a0", Cols: 120, Rows: 40}
	payload, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"terminal_id":"t-1"`, `"task_id":"task-1"`, `"session":"ctx-foli-39-01a0"`, `"cols":120`, `"rows":40`} {
		if !strings.Contains(string(payload), key) {
			t.Fatalf("payload %s lacks %s", payload, key)
		}
	}
	raw, err := json.Marshal(Message{Type: EventDaemonTerminalOpen, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	var out TerminalOpenPayload
	if err := json.Unmarshal(msg.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if msg.Type != EventDaemonTerminalOpen || out != in {
		t.Fatalf("round trip = %q %+v", msg.Type, out)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

`$GOTEST 'go test ./pkg/protocol -run TestTerminalOpenFrameRoundTrip -count=1'` — expected: compile error, `undefined: EventDaemonTerminalOpen`.

- [ ] **Step 3: Implement**

events.go, directly after `EventDaemonPendingWork = "daemon:pending_work"`:

```go
	// EventDaemonTerminalOpen asks the daemon to attach one tmux client for a
	// browser viewer of a tmux-mode task and dial back
	// /api/daemon/terminals/{terminal_id}/ws with the bytes (ContextPRO fork,
	// spec 2026-09-03). A hint like task_available: it carries no bytes, and a
	// lost frame only means the viewer sees "runtime unreachable" and retries.
	EventDaemonTerminalOpen = "daemon:terminal_open"
```

messages.go, directly after `DaemonCapabilityLocalTmuxV1 = "local-tmux-v1"`:

```go
	// DaemonCapabilityLocalTmuxTerminalV1 advertises that the daemon can serve
	// a live terminal for its tmux-mode tasks: it handles daemon:terminal_open
	// by attaching a tmux client inside a pseudo-terminal and dialing back the
	// terminal socket. Advertised only when tmux resolves AND a PTY can be
	// allocated, so the server can tell the viewer "update the daemon" instead
	// of waiting on a daemon that will never dial back.
	DaemonCapabilityLocalTmuxTerminalV1 = "local-tmux-terminal-v1"
```

messages.go, directly after the `PendingWorkPayload` struct:

```go
// TerminalOpenPayload is sent from server to daemon when a viewer opens the
// terminal of a tmux-mode task. TerminalID is the server-minted id the daemon
// dials back with; Session is the tmux session the daemon must verify against
// its own task state before attaching; Cols/Rows size the first attach.
type TerminalOpenPayload struct {
	TerminalID string `json:"terminal_id"`
	TaskID     string `json:"task_id"`
	Session    string `json:"session"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
}
```

- [ ] **Step 4: Run the test and vet**

`$GOTEST 'gofmt -l ./pkg/protocol; go vet ./pkg/protocol && go test ./pkg/protocol -count=1'` — expected: `ok`, and `gofmt -l` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add server/pkg/protocol/events.go server/pkg/protocol/messages.go server/pkg/protocol/terminal_test.go
git commit -m "feat(protocol): terminal_open event and local-tmux-terminal-v1 capability"
```

---

### Task 2: Daemon advertises the capability when tmux and a PTY exist

**Files:**
- Modify: `server/go.mod`, `server/go.sum` (adds `github.com/creack/pty v1.1.24`)
- Modify: `server/internal/daemon/client.go` (after `tmuxAvailable`, lines ~185-195; `daemonClientCapabilities`, lines ~203-217)
- Test: `server/internal/daemon/tmux_capability_test.go` (append)

**Interfaces:**
- Produces: `var ptyOpen func() (*os.File, *os.File, error)`, `func ptyAvailable() bool`, `func resetPTYProbe()` (tests only), `local-tmux-terminal-v1` appended to `X-Client-Capabilities` when both tmux and a PTY are available.

- [ ] **Step 1: Write the failing test** (append; make sure the file imports `os`, `errors`, `strings`, `testing`, and `protocol`)

```go
// The terminal needs both tmux and a pseudo-terminal; advertising it without a
// PTY would make the server hand out terminal_open hints the daemon can never
// honour.
func TestDaemonClientCapabilitiesAdvertiseTerminalOnlyWithTmuxAndPTY(t *testing.T) {
	origLook, origOpen := tmuxLookPath, ptyOpen
	t.Cleanup(func() { tmuxLookPath, ptyOpen = origLook, origOpen; resetPTYProbe() })

	tmuxLookPath = func() (string, error) { return "/opt/homebrew/bin/tmux", nil }
	ptyOpen = func() (*os.File, *os.File, error) { return nil, nil, errors.New("openpty: not permitted") }
	resetPTYProbe()
	if strings.Contains(daemonClientCapabilities(), protocol.DaemonCapabilityLocalTmuxTerminalV1) {
		t.Fatal("terminal capability advertised without a PTY")
	}

	ptyOpen = func() (*os.File, *os.File, error) {
		m, err := os.CreateTemp(t.TempDir(), "pty-m")
		if err != nil {
			return nil, nil, err
		}
		s, err := os.CreateTemp(t.TempDir(), "pty-s")
		if err != nil {
			return nil, nil, err
		}
		return m, s, nil
	}
	resetPTYProbe()
	if !strings.Contains(daemonClientCapabilities(), protocol.DaemonCapabilityLocalTmuxTerminalV1) {
		t.Fatal("terminal capability missing with tmux and a PTY")
	}

	tmuxLookPath = func() (string, error) { return "", errors.New("tmux: not found") }
	resetPTYProbe()
	if strings.Contains(daemonClientCapabilities(), protocol.DaemonCapabilityLocalTmuxTerminalV1) {
		t.Fatal("terminal capability advertised without tmux")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

`$GOTEST 'go test ./internal/daemon -run TestDaemonClientCapabilities -count=1'` — expected: compile error `undefined: ptyOpen`.

- [ ] **Step 3: Add the dependency and implement**

```bash
$GOTEST 'go get github.com/creack/pty@v1.1.24 >/dev/null && grep creack go.mod'
```

client.go — add imports `"os"` and `"github.com/creack/pty"` (`sync` is already imported), then after `tmuxAvailable`:

```go
// ptyOpen allocates one pseudo-terminal pair. Package-level so tests can force
// "unavailable" without touching /dev/ptmx.
var ptyOpen = func() (*os.File, *os.File, error) { return pty.Open() }

var (
	ptyProbeOnce sync.Once
	ptyProbeOK   bool
)

// ptyAvailable reports whether this machine can allocate a pseudo-terminal,
// which the live terminal needs for the per-viewer `tmux attach` client.
// Probed once per process: PTY support does not come and go while the daemon
// runs, and the probe opens a device node.
func ptyAvailable() bool {
	ptyProbeOnce.Do(func() {
		m, s, err := ptyOpen()
		if err != nil {
			return
		}
		_ = m.Close()
		_ = s.Close()
		ptyProbeOK = true
	})
	return ptyProbeOK
}

// resetPTYProbe forgets the cached probe. Tests only.
func resetPTYProbe() {
	ptyProbeOnce = sync.Once{}
	ptyProbeOK = false
}
```

In `daemonClientCapabilities`, replace the tmux block with:

```go
	if tmuxAvailable() {
		caps = append(caps, protocol.DaemonCapabilityLocalTmuxV1)
		// The live terminal additionally needs a pseudo-terminal for the
		// per-viewer tmux client (spec 2026-09-03).
		if ptyAvailable() {
			caps = append(caps, protocol.DaemonCapabilityLocalTmuxTerminalV1)
		}
	}
```

- [ ] **Step 4: Run the tests**

`$GOTEST 'go mod tidy >/dev/null; gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "TestDaemonClientCapabilities" -count=1'` — expected `ok`. `go.mod` must now list `github.com/creack/pty v1.1.24` without `// indirect`.

- [ ] **Step 5: Commit**

```bash
git add server/go.mod server/go.sum server/internal/daemon/client.go server/internal/daemon/tmux_capability_test.go
git commit -m "feat(daemon): advertise local-tmux-terminal-v1 when tmux and a PTY are available"
```

---

### Task 3: tmux controller — `Attach` inside a PTY, `window-size latest`

**Files:**
- Modify: `server/internal/daemon/tmux_exec.go`
- Modify: `server/internal/daemon/tmux_runner_test.go` (fakeTmux gains a stub `Attach`, lines ~137-197)
- Test: `server/internal/daemon/tmux_exec_test.go`

**Interfaces:**
- Produces:
  ```go
  type tmuxClient interface { io.ReadWriteCloser; Resize(cols, rows int) error; Wait() int }
  // added to tmuxController:
  Attach(name string, cols, rows int) (tmuxClient, error)
  ```
  `execTmux.NewSession` additionally runs `set-option -w -t =name: window-size latest`.

- [ ] **Step 1: Extend the fake tmux script and write the failing tests**

In `installFakeTmux` (tmux_exec_test.go, the `case "$1" in` block) add two arms before `*)`:

```sh
  set-option) exit 0 ;;
  attach-session) name="${3#=}"; [ -f "$FAKE_TMUX_SESSIONS/$name" ] || { echo "can't find session: $name" >&2; exit 1; }; exec cat ;;
```

In the existing argv assertion list (the `for _, want := range []string{` block, lines ~79-85) add the line `"set-option -w -t =ctx-x-1: window-size latest",` after the `new-session` entry.

Append to tmux_exec_test.go (imports needed: `context`, `io`, `strings`, `time`, `github.com/creack/pty`):

```go
// Attach runs `tmux attach-session` inside a pseudo-terminal: what the viewer
// types goes in, what tmux draws comes out, and Resize changes the PTY size the
// tmux client sees. The fake tmux execs cat, so typed bytes come straight back.
func TestExecTmuxAttachRunsTheClientInsideAPTY(t *testing.T) {
	installFakeTmux(t)
	ctl, err := newExecTmux()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := ctl.NewSession(ctx, "ctx-x-2", t.TempDir(), []string{"sh", "/tmp/run.sh"}); err != nil {
		t.Fatal(err)
	}
	client, err := ctl.Attach("ctx-x-2", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if got := readUntil(client, "hello", 5*time.Second); !strings.Contains(got, "hello") {
		t.Fatalf("pty output %q lacks the echoed input", got)
	}
	if err := client.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	size, err := pty.GetsizeFull(client.(*ptyTmuxClient).f)
	if err != nil || size.Cols != 100 || size.Rows != 30 {
		t.Fatalf("pty size after Resize = %+v (%v)", size, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, client, 5*time.Second)
}

// A missing session makes tmux exit non-zero; the client must surface that
// through Wait so the daemon can tell the viewer instead of hanging.
func TestExecTmuxAttachMissingSessionExitsNonZero(t *testing.T) {
	installFakeTmux(t)
	ctl, err := newExecTmux()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ctl.Attach("ctx-missing", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if code := waitExit(t, client, 5*time.Second); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func readUntil(r io.Reader, want string, timeout time.Duration) string {
	var got strings.Builder
	buf := make([]byte, 256)
	deadline := time.Now().Add(timeout)
	for !strings.Contains(got.String(), want) && time.Now().Before(deadline) {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return got.String()
}

func waitExit(t *testing.T, client tmuxClient, timeout time.Duration) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- client.Wait() }()
	select {
	case code := <-done:
		return code
	case <-time.After(timeout):
		t.Fatal("attach client did not exit")
		return -1
	}
}
```

In tmux_runner_test.go add a stub so the fake still satisfies the interface (Task 5 replaces it):

```go
func (f *fakeTmux) Attach(name string, cols, rows int) (tmuxClient, error) {
	return nil, errors.New("fakeTmux: Attach not configured")
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/daemon -run TestExecTmux -count=1'` — expected: compile error `undefined: tmuxClient`.

- [ ] **Step 3: Implement**

tmux_exec.go — imports become `bytes`, `context`, `errors`, `fmt`, `io`, `os`, `os/exec`, `time`, `github.com/creack/pty`. Add to the `tmuxController` interface:

```go
	// Attach starts one tmux client for name inside a pseudo-terminal sized
	// cols×rows. Each browser viewer gets its own client, so tmux repaints
	// the whole screen for every attach and no scrollback replay is needed.
	Attach(name string, cols, rows int) (tmuxClient, error)
```

Add the client type and the implementation:

```go
// tmuxClient is one attached tmux client running inside a pseudo-terminal:
// what a browser viewer types goes to Write, what tmux draws comes out of
// Read, Resize follows the viewer's terminal size, Close detaches (SIGHUP to
// the client; the session itself is untouched) and Wait returns the client's
// exit code once it has ended.
type tmuxClient interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() int
}

// Attach deliberately takes no context: the client's life is the viewer's
// connection, ended through Close, and a context cancel would kill tmux
// mid-write instead of detaching cleanly.
func (t *execTmux) Attach(name string, cols, rows int) (tmuxClient, error) {
	cmd := exec.Command(t.path, "attach-session", "-t", "="+name)
	// xterm.js emulates xterm with 256 colours; tell tmux so it draws for it.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("tmux attach-session %s: %w", name, err)
	}
	c := &ptyTmuxClient{cmd: cmd, f: f, done: make(chan struct{})}
	go func() {
		c.code = exitCodeOf(cmd.Wait())
		close(c.done)
	}()
	return c, nil
}

type ptyTmuxClient struct {
	cmd  *exec.Cmd
	f    *os.File
	done chan struct{}
	code int
}

func (c *ptyTmuxClient) Read(p []byte) (int, error)  { return c.f.Read(p) }
func (c *ptyTmuxClient) Write(p []byte) (int, error) { return c.f.Write(p) }

func (c *ptyTmuxClient) Resize(cols, rows int) error {
	return pty.Setsize(c.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close hangs up the pseudo-terminal, which tmux answers by detaching this
// client. A client that ignores the hangup is killed after a grace period so a
// viewer can never leak a process.
func (c *ptyTmuxClient) Close() error {
	err := c.f.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
	return err
}

func (c *ptyTmuxClient) Wait() int {
	<-c.done
	return c.code
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
```

Replace `NewSession` with:

```go
func (t *execTmux) NewSession(ctx context.Context, name, folder string, command []string) error {
	args := append([]string{"new-session", "-d", "-s", name, "-c", folder, "--"}, command...)
	if err := t.run(ctx, args...); err != nil {
		return err
	}
	// Every browser viewer is its own tmux client, so the window must follow
	// the client that resized last rather than shrink to the smallest one
	// (tmux's default before 3.1, and still a common user setting). Best
	// effort: the session works without it, only multi-viewer sizing differs.
	_ = t.run(ctx, "set-option", "-w", "-t", paneTarget(name), "window-size", "latest")
	return nil
}
```

- [ ] **Step 4: Run the tests**

`$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "TestExecTmux|Tmux" -count=1'` — expected `ok` (the fake execs `cat`, no agent CLI runs, so root is fine here).

- [ ] **Step 5: Commit**

```bash
git add server/internal/daemon/tmux_exec.go server/internal/daemon/tmux_exec_test.go server/internal/daemon/tmux_runner_test.go
git commit -m "feat(daemon): attach tmux clients inside a PTY and pin window-size latest"
```

---

### Task 4: Runner — fixed Claude session id, resume, session report, "closed" completes

**Files:**
- Modify: `server/internal/daemon/tmux_runner.go` (`tmuxState` ~114-130; launch section ~235-270; `adoptTmuxSessions` ~347-384; `tmuxOutcome` ~324-345)
- Modify: `server/internal/daemon/client.go` (after `ReportProgress`, ~line 496)
- Test: `server/internal/daemon/tmux_runner_test.go`, `server/internal/daemon/client_tmux_test.go` (new)

**Interfaces:**
- Consumes: `Task.PriorSessionID` (types.go:122), `agent.BuildClaudeInteractiveArgs`, `readTmuxState`/`writeTmuxState`.
- Produces: `tmuxState.ClaudeSessionID` (json `claude_session_id`); `(*Client).ReportTmuxSession(ctx, taskID, session, claudeSessionID string) error` posting `{"session","claude_session_id"}` to `POST /api/daemon/tasks/{taskID}/tmux`; `(*Daemon).reportTmuxSession(ctx, st tmuxState, log *slog.Logger)`; `TaskResult.SessionID` set on completed results; a vanished session without exit code → `Status "completed"`, comment prefix `Session closed before Claude Code exited.`

- [ ] **Step 1: Write the failing tests**

(a) In `TestRunTmuxTaskSpawnsSessionAndCompletesOnExitZero`, right after the existing run.sh assertions (while the session is still alive), add — the file needs `regexp` imported:

```go
	// The conversation id is chosen up front so the task can store it.
	sessionID := regexp.MustCompile(`'--session-id' '([0-9a-f-]{36})'`).FindStringSubmatch(string(script))
	if sessionID == nil {
		t.Fatalf("run.sh does not pin a session id:\n%s", script)
	}
	if st, err := readTmuxState(taskDir); err != nil || st.ClaudeSessionID != sessionID[1] {
		t.Fatalf("state claude_session_id = %q (%v), want %s", st.ClaudeSessionID, err, sessionID[1])
	}
```

and after the existing result assertions at the end of that test:

```go
	if result.SessionID != sessionID[1] {
		t.Fatalf("result session id = %q, want %s", result.SessionID, sessionID[1])
	}
```

(b) New test in tmux_runner_test.go:

```go
// A follow-up task on the issue carries the prior Claude session; the launch
// must resume it instead of pinning a fresh id (spec 2026-09-03 §Lifecycle 3).
func TestRunTmuxTaskResumesThePriorSession(t *testing.T) {
	orig := tmuxPollInterval
	tmuxPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tmuxPollInterval = orig })
	ctl := newFakeTmux()
	d := newTmuxTestDaemon(t, ctl)
	folder := t.TempDir()
	task := Task{ID: "resume01-task", IssueID: "issue-2", IssueIdentifier: "CTX-8", PriorSessionID: "9b979dbb-69f4-42c0-a02f-026ce67a39b7"}
	assignment := &localDirectoryAssignment{Ref: localDirectoryRef{LocalPath: folder, DaemonID: "d-tmux", ExecutionMode: "tmux"}, AbsPath: folder, RealPath: folder}
	env := &execenv.Environment{WorkDir: folder, RootDir: filepath.Join(d.cfg.WorkspacesRoot, "env-2")}
	done := make(chan struct{})
	var result TaskResult
	go func() {
		defer close(done)
		result, _ = d.runTmuxTask(context.Background(), task, env, assignment, "/opt/fake/claude", agent.ExecOptions{}, nil, "Continue", slog.Default())
	}()
	var name string
	for deadline := time.Now().Add(2 * time.Second); name == "" && time.Now().Before(deadline); {
		name = ctl.firstAlive()
		time.Sleep(5 * time.Millisecond)
	}
	if name == "" {
		t.Fatal("session never started")
	}
	taskDir := tmuxTaskDir(d.cfg.WorkspacesRoot, task.ID)
	script, _ := os.ReadFile(filepath.Join(taskDir, "run.sh"))
	if !strings.Contains(string(script), "'--resume' '9b979dbb-69f4-42c0-a02f-026ce67a39b7'") || strings.Contains(string(script), "--session-id") {
		t.Fatalf("run.sh should resume the prior session and not pin a new id:\n%s", script)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "exit-code"), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctl.end(name)
	<-done
	if result.SessionID != task.PriorSessionID {
		t.Fatalf("result session id = %q, want the resumed id", result.SessionID)
	}
}

// Closing the session by any means (kill-session, Close session on the issue
// page, tmux stopping) is how the operator finishes a tmux task; it is a
// completed run, not a lost one (spec 2026-09-03 §Lifecycle 1).
func TestTmuxOutcomeCompletesWhenTheSessionClosedWithoutAnExitCode(t *testing.T) {
	dir := t.TempDir()
	st := tmuxState{Session: "ctx-x-9", ExitCodePath: filepath.Join(dir, "exit-code"), TranscriptPath: filepath.Join(dir, "transcript.log"), ScreenPath: filepath.Join(dir, "screen.txt"), WorkDir: "/w", ClaudeSessionID: "sid-9"}
	if err := os.WriteFile(st.ScreenPath, []byte("⏺ working\nline\nline\nline\nline\nline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := tmuxOutcome(st)
	if err != nil || result.Status != "completed" || !strings.HasPrefix(result.Comment, "Session closed before Claude Code exited.\n\n⏺ working") || result.SessionID != "sid-9" || result.WorkDir != "/w" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}
```

(c) Flip the two existing "session lost" expectations. At tmux_runner_test.go:306 the watch of `mk("gone-lost", "")` must now succeed:

```go
	if res, err := d.watchTmuxSession(context.Background(), ctl, mk("gone-lost", ""), slog.Default()); err != nil || res.Status != "completed" || !strings.HasPrefix(res.Comment, "Session closed before Claude Code exited.") {
		t.Fatalf("a session that vanished without an exit code must complete, got %+v (%v)", res, err)
	}
```

At line ~408 (adoption test) change the `settled["lost"]` clause from `!strings.Contains(settled["lost"], "session lost")` to `settled["lost"] != "completed"`. Read how `settled` is filled a few lines above and keep its shape.

(d) New file `client_tmux_test.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientReportTmuxSessionPostsSessionAndClaudeID(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.SetToken("mdt_test")
	if err := c.ReportTmuxSession(context.Background(), "task-1", "ctx-foli-39-01a0", "sid-1"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/daemon/tasks/task-1/tmux" || gotAuth != "Bearer mdt_test" || gotBody["session"] != "ctx-foli-39-01a0" || gotBody["claude_session_id"] != "sid-1" {
		t.Fatalf("path=%s auth=%s body=%v", gotPath, gotAuth, gotBody)
	}
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/daemon -run "Tmux|TestClientReportTmux" -count=1 2>&1 | tail -5'` — expected: compile errors (`st.ClaudeSessionID undefined`, `c.ReportTmuxSession undefined`).

- [ ] **Step 3: Implement**

client.go, after `ReportProgress`:

```go
// ReportTmuxSession records the live tmux session and the Claude Code
// conversation id on the task (ContextPRO fork). The issue page needs the
// session to offer a terminal, and the id must reach the server before the
// task can be completed from the server side (Close session, card moved to
// done), when the daemon's own completion report is already a no-op.
func (c *Client) ReportTmuxSession(ctx context.Context, taskID, session, claudeSessionID string) error {
	return c.postJSON(ctx, fmt.Sprintf("/api/daemon/tasks/%s/tmux", taskID), map[string]any{
		"session":           session,
		"claude_session_id": claudeSessionID,
	}, nil)
}
```

tmux_runner.go — import `github.com/google/uuid`. In `tmuxState` add after `ScreenPath`:

```go
	// ClaudeSessionID is the conversation id Claude Code was launched with
	// (--session-id, or --resume of a prior task). Reported to the server and
	// returned in the task result so the run can be resumed later.
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
```

In `runTmuxTask`, replace `args := agent.BuildClaudeInteractiveArgs(opts, mcpPath, d.logger)` with:

```go
	// The conversation id is fixed before launch (spec 2026-09-03): a follow-up
	// task on the issue resumes the prior session, a fresh one gets an id we
	// choose, so the task can store the id without scraping Claude's goodbye
	// text. Resume is best effort: Claude Code starts a copy under a new id
	// when the original cannot be continued, and this reports the id it was
	// asked to use.
	claudeSessionID := task.PriorSessionID
	sessionArgs := []string{"--resume", claudeSessionID}
	if claudeSessionID == "" {
		claudeSessionID = uuid.NewString()
		sessionArgs = []string{"--session-id", claudeSessionID}
	}
	args := append(agent.BuildClaudeInteractiveArgs(opts, mcpPath, d.logger), sessionArgs...)
```

Add `ClaudeSessionID: claudeSessionID,` to the `st := tmuxState{...}` literal. After the `taskLog.Info("tmux: interactive session started", ...)` line and BEFORE `d.announceTmuxSession(...)` insert `d.reportTmuxSession(ctx, st, taskLog)` — the order matters: the announce posts progress, and the `task:progress` event is what makes the web refetch the task list, which must already carry `tmux_session`. Add the method next to `announceTmuxSession`:

```go
// reportTmuxSession is best effort: a failed report hides the terminal until
// adoption re-reports it, and the run itself is unaffected. Nil client in tests.
func (d *Daemon) reportTmuxSession(ctx context.Context, st tmuxState, log *slog.Logger) {
	if d.client == nil {
		return
	}
	if err := d.client.ReportTmuxSession(ctx, st.TaskID, st.Session, st.ClaudeSessionID); err != nil {
		log.Warn("tmux: reporting the session to the server failed", "session", st.Session, "error", err)
	}
}
```

In the `adoptTmuxSessions` goroutine, before `result, err := d.watchTmuxSession(...)`, add `d.reportTmuxSession(ctx, st, log)` (a daemon restart lost nothing server-side, but re-reporting is cheap and covers a report that failed before the restart).

Replace `tmuxOutcome`'s switch and return with:

```go
	switch {
	case err != nil:
		return TaskResult{}, fmt.Errorf("interactive session %s: %w", st.Session, err)
	case !found:
		// The session vanished without Claude Code writing an exit code:
		// killed with tmux, closed from the issue page, or the tmux server
		// stopped. The operator's rule (spec 2026-09-03): closing the session
		// means the work is done, so this completes rather than fails.
		return TaskResult{
			Status:    "completed",
			Comment:   fmt.Sprintf("Session closed before Claude Code exited.\n\n%s", tail),
			WorkDir:   st.WorkDir,
			EnvRoot:   st.EnvRoot,
			SessionID: st.ClaudeSessionID,
		}, nil
	case code != 0:
		return TaskResult{}, fmt.Errorf("interactive session %s exited with code %d\n\n%s", st.Session, code, tail)
	}
	return TaskResult{
		Status:    "completed",
		Comment:   fmt.Sprintf("Interactive session %s finished.\n\n%s", st.Session, tail),
		WorkDir:   st.WorkDir,
		EnvRoot:   st.EnvRoot,
		SessionID: st.ClaudeSessionID,
	}, nil
```

(A failed run returns an error and therefore stores no session id; Task 14 aligns the spec sentence that said "all three cases".)

- [ ] **Step 4: Run the tests**

`$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "Tmux|TestClientReportTmux" -count=1'` — expected `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/daemon/tmux_runner.go server/internal/daemon/tmux_runner_test.go server/internal/daemon/client.go server/internal/daemon/client_tmux_test.go
git commit -m "feat(daemon): pin the Claude session id, report the tmux session, complete on manual close"
```

---

### Task 5: Daemon handles `terminal_open` and dials back

**Files:**
- Create: `server/internal/daemon/terminal.go`
- Modify: `server/internal/daemon/wakeup.go` (dispatch switch ~373-445; header block ~105-120; `taskWakeupURL` ~486-512)
- Modify: `server/internal/daemon/daemon.go` (struct fields next to `tmux`, ~595-600)
- Modify: `server/internal/daemon/tmux_runner_test.go` (real fake client replaces the Task 3 stub)
- Test: `server/internal/daemon/terminal_test.go`

**Interfaces:**
- Consumes: `protocol.EventDaemonTerminalOpen`, `protocol.TerminalOpenPayload`, `tmuxController.Attach`, `readTmuxState`, `tmuxTaskDir`, `d.client.Token()`, `d.client.platform/version/os`, `daemonClientCapabilities()`.
- Produces: `(*Daemon).handleTerminalOpen(p protocol.TerminalOpenPayload)`; `(*Daemon).daemonWSHeaders() http.Header`; `daemonWSBase(baseURL string) (*url.URL, error)`; frames on the dial-back socket: binary = bytes, text `{"type":"resize","cols","rows"}` inbound, text `{"type":"exit","code":N}` outbound.

- [ ] **Step 1: Replace the fake `Attach` and write the failing tests**

In tmux_runner_test.go add fields `attached []string` and `attachClient *fakeTmuxClient` to `fakeTmux`, replace the Task 3 stub with the real fake (imports `bytes`, `fmt`, `io` are needed):

```go
// fakeTmuxClient stands in for an attached tmux client: input written by the
// daemon lands in input, bytes pushed with emit() come out of Read, and exit()
// ends it with a code (Close hangs up with -1, like a real SIGHUP).
type fakeTmuxClient struct {
	mu      sync.Mutex
	input   bytes.Buffer
	resizes [][2]int
	out     *io.PipeReader
	outW    *io.PipeWriter
	done    chan struct{}
	code    int
	closed  bool
}

func newFakeTmuxClient() *fakeTmuxClient {
	r, w := io.Pipe()
	return &fakeTmuxClient{out: r, outW: w, done: make(chan struct{})}
}
func (c *fakeTmuxClient) Read(p []byte) (int, error) { return c.out.Read(p) }
func (c *fakeTmuxClient) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.input.Write(p)
}
func (c *fakeTmuxClient) Resize(cols, rows int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resizes = append(c.resizes, [2]int{cols, rows})
	return nil
}
func (c *fakeTmuxClient) Close() error { c.exit(-1); return nil }
func (c *fakeTmuxClient) Wait() int    { <-c.done; return c.code }
func (c *fakeTmuxClient) emit(s string) { _, _ = c.outW.Write([]byte(s)) }
func (c *fakeTmuxClient) exit(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.code = code
	_ = c.outW.Close()
	close(c.done)
}
func (c *fakeTmuxClient) typed() string { c.mu.Lock(); defer c.mu.Unlock(); return c.input.String() }
func (c *fakeTmuxClient) lastResize() [2]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.resizes) == 0 {
		return [2]int{}
	}
	return c.resizes[len(c.resizes)-1]
}

func (f *fakeTmux) Attach(name string, cols, rows int) (tmuxClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.alive[name] {
		return nil, errors.New("can't find session: " + name)
	}
	f.attached = append(f.attached, fmt.Sprintf("%s %dx%d", name, cols, rows))
	if f.attachClient == nil {
		f.attachClient = newFakeTmuxClient()
	}
	return f.attachClient, nil
}
```

New `terminal_test.go`:

```go
package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeTerminalServer plays the ContextPRO server end of the terminal socket:
// it accepts the daemon's dial-back and hands the connection to the test.
type fakeTerminalServer struct {
	srv   *httptest.Server
	conns chan *websocket.Conn
	paths chan string
	auths chan string
}

func newFakeTerminalServer(t *testing.T) *fakeTerminalServer {
	t.Helper()
	f := &fakeTerminalServer{conns: make(chan *websocket.Conn, 4), paths: make(chan string, 4), auths: make(chan string, 4)}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.paths <- r.URL.Path
		f.auths <- r.Header.Get("Authorization")
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.conns <- conn
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTerminalTestDaemon(t *testing.T, ctl *fakeTmux, serverURL string) *Daemon {
	t.Helper()
	d := newTmuxTestDaemon(t, ctl)
	d.cfg.ServerBaseURL = serverURL
	d.client = NewClient(serverURL)
	d.client.SetToken("mdt_terminal")
	return d
}

func writeLiveState(t *testing.T, d *Daemon, taskID, session string) {
	t.Helper()
	dir := tmuxTaskDir(d.cfg.WorkspacesRoot, taskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeTmuxState(dir, tmuxState{Session: session, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForAttach(t *testing.T, ctl *fakeTmux) *fakeTmuxClient {
	t.Helper()
	var client *fakeTmuxClient
	waitFor(t, func() bool {
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		client = ctl.attachClient
		return client != nil
	}, "tmux attach")
	return client
}

func TestHandleTerminalOpenDialsBackAndBridgesBytes(t *testing.T) {
	ctl := newFakeTmux()
	ctl.alive["ctx-t-1"] = true
	server := newFakeTerminalServer(t)
	d := newTerminalTestDaemon(t, ctl, server.srv.URL)
	writeLiveState(t, d, "task-1", "ctx-t-1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.handleTerminalOpen(protocol.TerminalOpenPayload{TerminalID: "term-1", TaskID: "task-1", Session: "ctx-t-1", Cols: 100, Rows: 30})
	}()
	var conn *websocket.Conn
	select {
	case conn = <-server.conns:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon never dialed back")
	}
	defer conn.Close()
	if path := <-server.paths; path != "/api/daemon/terminals/term-1/ws" {
		t.Fatalf("dial path = %s", path)
	}
	if auth := <-server.auths; auth != "Bearer mdt_terminal" {
		t.Fatalf("auth header = %q", auth)
	}
	client := waitForAttach(t, ctl)
	ctl.mu.Lock()
	attached := ctl.attached[0]
	ctl.mu.Unlock()
	if attached != "ctx-t-1 100x30" {
		t.Fatalf("attach = %q", attached)
	}

	// Server → daemon: keystrokes reach the PTY, resize reaches tmux.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":132,"rows":43}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return client.typed() == "ls\r" && client.lastResize() == [2]int{132, 43} }, "input and resize to reach the tmux client")

	// Daemon → server: PTY output arrives as binary frames.
	client.emit("$ ls\r\nHELLO.md\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "$ ls\r\nHELLO.md\r\n" {
		t.Fatalf("frame = %d %q (%v)", kind, data, err)
	}

	// tmux client exit → exit frame, then the socket closes.
	client.exit(0)
	kind, data, err = conn.ReadMessage()
	if err != nil || kind != websocket.TextMessage || string(data) != `{"type":"exit","code":0}` {
		t.Fatalf("exit frame = %d %q (%v)", kind, data, err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("socket should close after the exit frame")
	}
	<-done
}

func TestHandleTerminalOpenRefusesUnknownTasks(t *testing.T) {
	ctl := newFakeTmux()
	ctl.alive["ctx-other"] = true
	server := newFakeTerminalServer(t)
	d := newTerminalTestDaemon(t, ctl, server.srv.URL)
	writeLiveState(t, d, "task-1", "ctx-t-1")

	// Wrong session name for a known task, and an unknown task: neither dials.
	d.handleTerminalOpen(protocol.TerminalOpenPayload{TerminalID: "term-2", TaskID: "task-1", Session: "ctx-other", Cols: 80, Rows: 24})
	d.handleTerminalOpen(protocol.TerminalOpenPayload{TerminalID: "term-3", TaskID: "nope", Session: "ctx-other", Cols: 80, Rows: 24})
	select {
	case <-server.conns:
		t.Fatal("daemon dialed back for a task it does not run")
	case <-time.After(300 * time.Millisecond):
	}
	if len(ctl.attached) != 0 {
		t.Fatalf("attached = %v", ctl.attached)
	}
}

func TestHandleTerminalOpenViewerLeavingDetachesOnly(t *testing.T) {
	ctl := newFakeTmux()
	ctl.alive["ctx-t-1"] = true
	server := newFakeTerminalServer(t)
	d := newTerminalTestDaemon(t, ctl, server.srv.URL)
	writeLiveState(t, d, "task-1", "ctx-t-1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.handleTerminalOpen(protocol.TerminalOpenPayload{TerminalID: "term-4", TaskID: "task-1", Session: "ctx-t-1", Cols: 80, Rows: 24})
	}()
	conn := <-server.conns
	client := waitForAttach(t, ctl)
	conn.Close() // the viewer went away
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not finish after the socket closed")
	}
	if code := client.Wait(); code != -1 {
		t.Fatalf("client should have been hung up (code -1), got %d", code)
	}
	if len(ctl.killed) != 0 || !ctl.alive["ctx-t-1"] {
		t.Fatal("the tmux session must survive a viewer leaving")
	}
}

func TestHandleTerminalOpenEnforcesTheAttachLimit(t *testing.T) {
	ctl := newFakeTmux()
	ctl.alive["ctx-t-1"] = true
	server := newFakeTerminalServer(t)
	d := newTerminalTestDaemon(t, ctl, server.srv.URL)
	writeLiveState(t, d, "task-1", "ctx-t-1")
	d.terminalAttached = terminalMaxAttaches
	d.handleTerminalOpen(protocol.TerminalOpenPayload{TerminalID: "term-5", TaskID: "task-1", Session: "ctx-t-1", Cols: 80, Rows: 24})
	select {
	case <-server.conns:
		t.Fatal("daemon dialed back past the attach limit")
	case <-time.After(300 * time.Millisecond):
	}
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/daemon -run TestHandleTerminalOpen -count=1 2>&1 | tail -3'` — expected: compile error `d.handleTerminalOpen undefined`.

- [ ] **Step 3: Implement**

daemon.go — next to the `tmux`/`tmuxAdoptionReport` fields:

```go
	// terminalMu / terminalAttached bound live-terminal attaches per daemon
	// (ContextPRO fork): each browser viewer is one PTY and one tmux client.
	terminalMu       sync.Mutex
	terminalAttached int
```

wakeup.go — (1) in `readTaskWakeupMessagesForConnection`, add a switch arm after the `EventDaemonPendingWork` arm:

```go
		case protocol.EventDaemonTerminalOpen:
			var payload protocol.TerminalOpenPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil || payload.TerminalID == "" || payload.TaskID == "" {
				d.logger.Debug("terminal open websocket invalid payload", "error", err)
				continue
			}
			// Own goroutine: the attach lives as long as the viewer, and the
			// read pump must stay free for the next frame.
			go d.handleTerminalOpen(payload)
```

(2) Replace the header-building lines in `runTaskWakeupConnection` (`headers := http.Header{}` through `headers.Set("X-Client-Capabilities", ...)`) with `headers := d.daemonWSHeaders()` and add:

```go
// daemonWSHeaders is the identity every daemon-initiated WebSocket sends: the
// same token and X-Client-* headers as the HTTP path, so a claim built over WS
// gets identical capability gating (MUL-4257) and the terminal dial-back is
// authenticated like the wakeup socket.
func (d *Daemon) daemonWSHeaders() http.Header {
	headers := http.Header{}
	if token := d.client.Token(); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if d.client.platform != "" {
		headers.Set("X-Client-Platform", d.client.platform)
	}
	if d.client.version != "" {
		headers.Set("X-Client-Version", d.client.version)
	}
	if d.client.os != "" {
		headers.Set("X-Client-OS", d.client.os)
	}
	headers.Set("X-Client-Capabilities", daemonClientCapabilities())
	return headers
}
```

(3) Split `taskWakeupURL` so the scheme mapping is shared:

```go
// daemonWSBase maps the configured server URL to its WebSocket origin.
func daemonWSBase(baseURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("invalid daemon server URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("daemon server URL must use http, https, ws, or wss")
	}
	u.RawPath = ""
	u.Fragment = ""
	return u, nil
}

func taskWakeupURL(baseURL string, runtimeIDs []string) (string, error) {
	u, err := daemonWSBase(baseURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/daemon/ws"
	q := u.Query()
	ids := append([]string(nil), runtimeIDs...)
	sort.Strings(ids)
	if len(ids) > 0 {
		q.Set("runtime_ids", strings.Join(ids, ","))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
```

New `terminal.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Live terminal for tmux-mode tasks (ContextPRO fork, spec 2026-09-03). The
// server sends daemon:terminal_open on behalf of one browser viewer; the daemon
// answers by attaching a tmux client inside a pseudo-terminal and dialing a
// dedicated WebSocket that carries the bytes. One attach per viewer; the tmux
// session itself is never ended from here — closing the PTY only detaches.
const (
	// terminalMaxAttaches bounds PTYs and tmux clients per daemon.
	terminalMaxAttaches = 12
	// terminalOutputChunk is the largest binary frame sent to the server.
	terminalOutputChunk   = 32 * 1024
	terminalWriteDeadline = 10 * time.Second
)

// terminalControl is the JSON body of a text frame on the terminal socket.
// Binary frames are raw bytes.
type terminalControl struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Code *int   `json:"code,omitempty"`
}

type terminalFrame struct {
	kind int
	data []byte
}

// handleTerminalOpen runs in its own goroutine per hint.
func (d *Daemon) handleTerminalOpen(p protocol.TerminalOpenPayload) {
	log := d.logger.With("terminal_id", p.TerminalID, "task_id", p.TaskID, "tmux_session", p.Session)
	// Only sessions this daemon started (or adopted) may be attached: the
	// state file is the record, and a name that does not match it is either
	// a stale hint or an attempt to reach an unrelated tmux session.
	st, err := readTmuxState(tmuxTaskDir(d.cfg.WorkspacesRoot, p.TaskID))
	if err != nil || st.Session != p.Session {
		log.Warn("terminal: refusing attach for a task this daemon is not running", "error", err)
		return
	}
	if !d.acquireTerminalSlot() {
		log.Warn("terminal: attach limit reached", "limit", terminalMaxAttaches)
		return
	}
	defer d.releaseTerminalSlot()

	ctx := d.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	// Dial first so the server learns quickly whether this daemon is reachable;
	// an attach failure is then reported as an exit frame instead of a timeout.
	conn, err := d.dialTerminal(ctx, p.TerminalID)
	if err != nil {
		log.Warn("terminal: dial back failed", "error", err)
		return
	}
	defer conn.Close()
	writes := make(chan terminalFrame, 64)
	writerDone := make(chan struct{})
	go runTerminalWriter(conn, writes, writerDone)

	ctl, err := d.tmuxController()
	var client tmuxClient
	if err == nil {
		client, err = ctl.Attach(p.Session, p.Cols, p.Rows)
	}
	if err != nil {
		log.Warn("terminal: tmux attach failed", "error", err)
		code := -1
		writes <- terminalFrame{websocket.TextMessage, mustJSON(terminalControl{Type: "exit", Code: &code})}
		close(writes)
		<-writerDone
		return
	}
	log.Info("terminal: attached")
	pumpTerminal(conn, client, writes, writerDone, log)
	log.Info("terminal: detached")
}

func (d *Daemon) acquireTerminalSlot() bool {
	d.terminalMu.Lock()
	defer d.terminalMu.Unlock()
	if d.terminalAttached >= terminalMaxAttaches {
		return false
	}
	d.terminalAttached++
	return true
}

func (d *Daemon) releaseTerminalSlot() {
	d.terminalMu.Lock()
	defer d.terminalMu.Unlock()
	d.terminalAttached--
}

// dialTerminal opens the per-viewer socket with the same identity headers and
// proxy handling as the wakeup socket.
func (d *Daemon) dialTerminal(ctx context.Context, terminalID string) (*websocket.Conn, error) {
	base, err := daemonWSBase(d.cfg.ServerBaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/daemon/terminals/" + url.PathEscape(terminalID) + "/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, _, err := dialer.DialContext(ctx, base.String(), d.daemonWSHeaders())
	return conn, err
}

// runTerminalWriter is the single writer for one socket (gorilla forbids
// concurrent writes). A failed write closes the socket and drains the queue so
// producers never block on a dead connection.
func runTerminalWriter(conn *websocket.Conn, writes <-chan terminalFrame, done chan<- struct{}) {
	defer close(done)
	for f := range writes {
		_ = conn.SetWriteDeadline(time.Now().Add(terminalWriteDeadline))
		if err := conn.WriteMessage(f.kind, f.data); err != nil {
			conn.Close()
			for range writes {
			}
			return
		}
	}
}

// pumpTerminal copies PTY output to the socket and socket input to the PTY
// until either side ends. The tmux client exiting (session ended, or tmux
// detached it) sends an exit frame so the viewer can show why; the viewer
// leaving hangs up the PTY, which detaches this client and nothing else.
func pumpTerminal(conn *websocket.Conn, client tmuxClient, writes chan terminalFrame, writerDone <-chan struct{}, log *slog.Logger) {
	ptyDone := make(chan struct{})
	go func() {
		defer close(ptyDone)
		buf := make([]byte, terminalOutputChunk)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				writes <- terminalFrame{websocket.BinaryMessage, chunk}
			}
			if err != nil {
				return // EOF or EIO once the client is gone
			}
		}
	}()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch kind {
			case websocket.BinaryMessage:
				if _, err := client.Write(data); err != nil {
					return
				}
			case websocket.TextMessage:
				var ctl terminalControl
				if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
					if err := client.Resize(ctl.Cols, ctl.Rows); err != nil {
						log.Debug("terminal: resize failed", "error", err)
					}
				}
			}
		}
	}()
	select {
	case <-ptyDone:
		code := client.Wait()
		writes <- terminalFrame{websocket.TextMessage, mustJSON(terminalControl{Type: "exit", Code: &code})}
	case <-readDone:
		_ = client.Close()
		<-ptyDone
	}
	// Order matters: the PTY reader has stopped before the queue is closed,
	// so nothing can send on a closed channel.
	close(writes)
	<-writerDone
	_ = client.Close()
	conn.Close()
	<-readDone
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return data
}
```

- [ ] **Step 4: Run the tests**

`$GOTEST 'gofmt -l ./internal/daemon; go vet ./internal/daemon && go test ./internal/daemon -run "TestHandleTerminalOpen|Tmux|Wakeup" -count=1'` — expected `ok`. Then the whole package once: `$GOTEST 'go test ./internal/daemon -count=1'` (use the non-root recipe if any test there spawns an agent CLI — the earlier tmux work needed it).

- [ ] **Step 5: Commit**

```bash
git add server/internal/daemon/terminal.go server/internal/daemon/terminal_test.go server/internal/daemon/wakeup.go server/internal/daemon/daemon.go server/internal/daemon/tmux_runner_test.go
git commit -m "feat(daemon): serve live terminals for tmux tasks over a dial-back socket"
```

---

### Task 6: Hub hands the `terminal_open` hint to one capable daemon

**Files:**
- Modify: `server/internal/daemonws/hub.go` (after `NotifyPendingWork`, ~line 335; frame builders ~541-584)
- Test: `server/internal/daemonws/terminal_test.go`

**Interfaces:**
- Consumes: `h.byRuntime map[string]map[*client]bool`, `client.identity.Capabilities` (raw comma-joined header), `client.send chan []byte`, `h.unregister(c)`, `mustMarshalRaw`.
- Produces: `func (h *Hub) NotifyTerminalOpen(runtimeID string, payload protocol.TerminalOpenPayload) string` returning `""` when delivered, else `"runtime_offline"` or `"runtime_no_terminal"`; `func hasCapability(advertised, want string) bool`.

- [ ] **Step 1: Write the failing test**

```go
package daemonws

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// terminalTestClient registers a bare client on a runtime the way HandleWebSocket
// would, without a socket: NotifyTerminalOpen only touches the queue.
func terminalTestClient(h *Hub, runtimeID, caps string) *client {
	c := &client{hub: h, send: make(chan []byte, 1), identity: ClientIdentity{RuntimeIDs: []string{runtimeID}, Capabilities: caps}}
	h.mu.Lock()
	h.clients[c] = true
	if h.byRuntime[runtimeID] == nil {
		h.byRuntime[runtimeID] = map[*client]bool{}
	}
	h.byRuntime[runtimeID][c] = true
	h.mu.Unlock()
	return c
}

func TestNotifyTerminalOpenReportsWhyNoDaemonCanAttach(t *testing.T) {
	h := NewHub()
	payload := protocol.TerminalOpenPayload{TerminalID: "t-1", TaskID: "task-1", Session: "ctx-a-1", Cols: 80, Rows: 24}
	if got := h.NotifyTerminalOpen("rt-offline", payload); got != "runtime_offline" {
		t.Fatalf("offline runtime = %q", got)
	}
	old := terminalTestClient(h, "rt-old", "rpc-v1,local-tmux-v1")
	if got := h.NotifyTerminalOpen("rt-old", payload); got != "runtime_no_terminal" {
		t.Fatalf("daemon without the capability = %q", got)
	}
	if len(old.send) != 0 {
		t.Fatal("an incapable daemon must not receive the hint")
	}
}

func TestNotifyTerminalOpenHandsTheHintToOneCapableDaemon(t *testing.T) {
	h := NewHub()
	payload := protocol.TerminalOpenPayload{TerminalID: "t-1", TaskID: "task-1", Session: "ctx-a-1", Cols: 80, Rows: 24}
	a := terminalTestClient(h, "rt-1", "rpc-v1,local-tmux-v1,local-tmux-terminal-v1")
	b := terminalTestClient(h, "rt-1", "rpc-v1,local-tmux-v1,local-tmux-terminal-v1")
	if got := h.NotifyTerminalOpen("rt-1", payload); got != "" {
		t.Fatalf("capable runtime = %q", got)
	}
	if len(a.send)+len(b.send) != 1 {
		t.Fatalf("exactly one daemon connection must get the hint, got %d", len(a.send)+len(b.send))
	}
	var frame []byte
	select {
	case frame = <-a.send:
	case frame = <-b.send:
	}
	var msg protocol.Message
	if err := json.Unmarshal(frame, &msg); err != nil || msg.Type != protocol.EventDaemonTerminalOpen {
		t.Fatalf("frame = %s (%v)", frame, err)
	}
	var got protocol.TerminalOpenPayload
	if err := json.Unmarshal(msg.Payload, &got); err != nil || got != payload {
		t.Fatalf("payload = %+v (%v)", got, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/daemonws -run TestNotifyTerminalOpen -count=1 2>&1 | tail -3'` — expected: `h.NotifyTerminalOpen undefined`. (If `NewHub()` leaves `clients`/`byRuntime` nil, the test helper panics: initialise them in the helper with the same map types.)

- [ ] **Step 3: Implement** (hub.go; `strings` may need importing)

```go
// NotifyTerminalOpen asks one daemon serving runtimeID to attach a tmux client
// for a viewer (ContextPRO fork, spec 2026-09-03). It returns "" when the hint
// was handed to a capable daemon, otherwise the status reason the viewer
// should see: "runtime_offline" when no daemon is connected for the runtime,
// "runtime_no_terminal" when none of them advertises
// local-tmux-terminal-v1. Exactly one client receives the frame, because each
// hint is one attach: fanning out would open one tmux client per daemon
// connection for a single viewer.
func (h *Hub) NotifyTerminalOpen(runtimeID string, payload protocol.TerminalOpenPayload) string {
	if h == nil || runtimeID == "" {
		return "runtime_offline"
	}
	data, err := terminalOpenFrame(payload)
	if err != nil {
		return "runtime_offline"
	}
	h.mu.RLock()
	clients := h.byRuntime[runtimeID]
	connected := len(clients)
	var capable []*client
	for c := range clients {
		if hasCapability(c.identity.Capabilities, protocol.DaemonCapabilityLocalTmuxTerminalV1) {
			capable = append(capable, c)
		}
	}
	h.mu.RUnlock()
	if connected == 0 {
		return "runtime_offline"
	}
	if len(capable) == 0 {
		return "runtime_no_terminal"
	}
	for _, c := range capable {
		select {
		case c.send <- data:
			return ""
		default:
			// A daemon whose queue is full is treated like the slow clients
			// in notifyFrame: evicted, and the next capable one is tried.
			h.unregister(c)
			c.conn.Close()
		}
	}
	return "runtime_offline"
}

func terminalOpenFrame(payload protocol.TerminalOpenPayload) ([]byte, error) {
	return json.Marshal(protocol.Message{
		Type:    protocol.EventDaemonTerminalOpen,
		Payload: mustMarshalRaw(payload),
	})
}

// hasCapability reports whether the comma-separated X-Client-Capabilities
// value contains want.
func hasCapability(advertised, want string) bool {
	for _, part := range strings.Split(advertised, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the tests**

`$GOTEST 'gofmt -l ./internal/daemonws; go vet ./internal/daemonws && go test ./internal/daemonws -count=1'` — expected `ok`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/daemonws/hub.go server/internal/daemonws/terminal_test.go
git commit -m "feat(server): hand terminal_open hints to a capable daemon connection"
```

---

### Task 7: Migration 445, sqlc setter, daemon report endpoint, response field

**Files:**
- Create: `server/migrations/445_agent_task_queue_tmux_session.up.sql`, `server/migrations/445_agent_task_queue_tmux_session.down.sql`
- Modify: `server/pkg/db/queries/agent.sql` (append)
- Generate: `server/pkg/db/generated/*` via sqlc
- Modify: `server/internal/handler/agent.go` (`AgentTaskResponse` ~356-403, `taskToResponse` ~754-816)
- Create: `server/internal/handler/tmux_task.go`
- Modify: `server/cmd/server/router.go` (~line 1466, next to `/tasks/{taskId}/progress`)
- Test: `server/internal/handler/tmux_task_test.go`

**Interfaces:**
- Consumes: `requireDaemonTaskAccess`, `parseUUIDOrBadRequest`, `isNotFound`, `writeError`/`writeJSON`, `textToPtr`, test helpers `dbfx`, `newDaemonTokenRequest`, `newRequest`, `withURLParam`, `testutil.Call`.
- Produces: column `agent_task_queue.tmux_session TEXT`; query `SetAgentTaskTmuxSession :one` with params `ID pgtype.UUID, TmuxSession pgtype.Text, ClaudeSessionID pgtype.Text`; `AgentTaskResponse.TmuxSession *string` (json `tmux_session`, `null` when unset); `POST /api/daemon/tasks/{taskId}/tmux` → `(*Handler).ReportTaskTmuxSession`; request type `TaskTmuxSessionRequest{Session, ClaudeSessionID string}`.

- [ ] **Step 1: Write the failing tests**

```go
package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func tmuxTestAgent(t *testing.T) (agentID, runtimeID string) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&agentID, &runtimeID)
	return agentID, runtimeID
}

// The daemon reports the live tmux session (and Claude's conversation id) so the
// issue page can offer the terminal and the id survives a server-side close.
func TestReportTaskTmuxSessionStoresSessionAndClaudeID(t *testing.T) {
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "tmux terminal report", testutil.Cols{"priority": "medium", "number": 88201})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running"})

	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/tmux", map[string]any{
		"session": "ctx-foli-39-01a0", "claude_session_id": "86f808da-d52d-459b-a84c-edafa4ef24dd",
	}, testWorkspaceID, "test-tmux-report")
	req = withURLParam(req, "taskId", taskID)
	testutil.Call(t, testHandler.ReportTaskTmuxSession, req).Want(http.StatusOK)

	var session, claude string
	dbfx.QueryRow(t, `SELECT tmux_session, session_id FROM agent_task_queue WHERE id = $1`, taskID).Scan(&session, &claude)
	if session != "ctx-foli-39-01a0" || claude != "86f808da-d52d-459b-a84c-edafa4ef24dd" {
		t.Fatalf("stored session=%q claude=%q", session, claude)
	}

	// The user-facing task list carries the session name.
	list := newRequest("GET", "/api/issues/"+issueID+"/task-runs", nil)
	list = withURLParam(list, "id", issueID)
	var tasks []AgentTaskResponse
	testutil.Call(t, testHandler.ListTasksByIssue, list).Want(http.StatusOK).JSON(&tasks)
	if len(tasks) != 1 || tasks[0].TmuxSession == nil || *tasks[0].TmuxSession != "ctx-foli-39-01a0" {
		t.Fatalf("task-runs response = %+v", tasks)
	}
}

// A report for a task that already ended must not resurrect the terminal.
func TestReportTaskTmuxSessionRejectsFinishedTasks(t *testing.T) {
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "tmux terminal late report", testutil.Cols{"priority": "medium", "number": 88202})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "completed"})
	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/tmux", map[string]any{"session": "ctx-late"}, testWorkspaceID, "test-tmux-late")
	req = withURLParam(req, "taskId", taskID)
	testutil.Call(t, testHandler.ReportTaskTmuxSession, req).Want(http.StatusConflict)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND tmux_session IS NOT NULL`, taskID); n != 0 {
		t.Fatal("finished task gained a tmux session")
	}
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/handler -run TestReportTaskTmuxSession -count=1 2>&1 | tail -3'` — expected: `testHandler.ReportTaskTmuxSession undefined`.

- [ ] **Step 3: Migration and query**

`445_agent_task_queue_tmux_session.up.sql`:

```sql
-- Live tmux session of a tmux-mode task (ContextPRO fork, spec 2026-09-03).
-- Reported by the daemon right after it spawns (and again on adoption); the
-- issue page offers the terminal only while status = 'running' AND this is set.
-- Kept after the task ends as the record that the run was a tmux run. Read by
-- primary key only, so no index; no foreign key (repo rule).
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS tmux_session TEXT;
```

`445_agent_task_queue_tmux_session.down.sql`:

```sql
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS tmux_session;
```

Append to `server/pkg/db/queries/agent.sql`:

```sql
-- name: SetAgentTaskTmuxSession :one
-- Records the tmux session a tmux-mode task runs in and, when known, the
-- Claude Code conversation id, so both exist before the task can be finished
-- from the server side (ContextPRO fork, spec 2026-09-03). Only a live task is
-- updated: a report that arrives after completion must not resurrect the
-- terminal affordance (the caller maps no-rows to 409).
UPDATE agent_task_queue
SET tmux_session = $2,
    session_id = COALESCE(sqlc.narg('claude_session_id'), session_id)
WHERE id = $1 AND status IN ('dispatched', 'running')
RETURNING *;
```

Regenerate and migrate the test database:

```bash
make sqlc   # or: $GOTEST 'go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate'
$GOTEST 'go run ./cmd/migrate up >/dev/null; echo migrated'
```

`db.AgentTaskQueue` now has `TmuxSession pgtype.Text`; every `SELECT *` task query returns it.

- [ ] **Step 4: Handler, response field, route**

agent.go — in `AgentTaskResponse`, after `BranchName`:

```go
	// TmuxSession is the tmux session of a tmux-mode run (ContextPRO fork).
	// Present on finished runs too: it is how the UI knows the run can be
	// resumed as a session; live affordances are gated on Status == running.
	TmuxSession *string `json:"tmux_session"`
```

and in the `taskToResponse` literal: `TmuxSession: textToPtr(t.TmuxSession),`.

New `tmux_task.go`:

```go
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// tmux-mode task endpoints (ContextPRO fork, spec 2026-09-03).

// TaskTmuxSessionRequest is the daemon's report of a tmux-mode task's live
// session.
type TaskTmuxSessionRequest struct {
	Session         string `json:"session"`
	ClaudeSessionID string `json:"claude_session_id"`
}

// ReportTaskTmuxSession stores the tmux session name (and the Claude Code
// conversation id when the daemon knows it) on a live task. The id is stored
// now rather than at completion because the operator can finish a tmux task
// from the server side (Close session, card moved to done), at which point the
// daemon's own completion report is a no-op and the id would be lost.
func (h *Handler) ReportTaskTmuxSession(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	task, ok := h.requireDaemonTaskAccess(w, r, taskID)
	if !ok {
		return
	}
	var req TaskTmuxSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Session) == "" {
		writeError(w, http.StatusBadRequest, "session required")
		return
	}
	if _, err := h.Queries.SetAgentTaskTmuxSession(r.Context(), db.SetAgentTaskTmuxSessionParams{
		ID:              task.ID,
		TmuxSession:     pgtype.Text{String: req.Session, Valid: true},
		ClaudeSessionID: pgtype.Text{String: req.ClaudeSessionID, Valid: req.ClaudeSessionID != ""},
	}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusConflict, "task is not running")
			return
		}
		slog.Warn("set task tmux session failed", "task_id", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record tmux session")
		return
	}
	// No dedicated realtime event: the daemon posts a progress report right
	// after this call, and its task:progress event makes the web refetch the
	// task list, which now carries tmux_session.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
```

Check the import paths against a neighbouring handler file (`pgtype` and the generated `db` package aliases are the ones daemon.go uses).

router.go — inside the `/api/daemon` route block, after the `progress` line:

```go
		r.Post("/tasks/{taskId}/tmux", h.ReportTaskTmuxSession)
```

- [ ] **Step 5: Run the tests**

`$GOTEST 'gofmt -l ./internal/handler ./cmd/server; go vet ./internal/handler ./cmd/server && go test ./internal/handler -run "TestReportTaskTmuxSession|TestListTasksByIssue" -count=1'` — expected `ok`.

- [ ] **Step 6: Commit** (two commits: schema, then handler)

```bash
git add server/migrations/445_agent_task_queue_tmux_session.up.sql server/migrations/445_agent_task_queue_tmux_session.down.sql server/pkg/db/queries/agent.sql server/pkg/db/generated
git commit -m "feat(db): record the live tmux session on agent tasks"
git add server/internal/handler/agent.go server/internal/handler/tmux_task.go server/internal/handler/tmux_task_test.go server/cmd/server/router.go
git commit -m "feat(server): daemons report the tmux session of a running task"
```

---

### Task 8: The bridge package

**Files:**
- Create: `server/internal/terminalbridge/bridge.go`
- Test: `server/internal/terminalbridge/bridge_test.go`

**Interfaces:**
- Consumes: `protocol.TerminalOpenPayload`, gorilla `*websocket.Conn`, `github.com/google/uuid`.
- Produces:
  ```go
  type Opener interface { NotifyTerminalOpen(runtimeID string, payload protocol.TerminalOpenPayload) string }
  type Viewer struct { TaskID, RuntimeID, Session, UserID string; Cols, Rows int }
  func New(opener Opener) *Bridge
  func (b *Bridge) ServeViewer(ctx context.Context, conn *websocket.Conn, v Viewer)   // owns and closes conn
  func (b *Bridge) ServeDaemon(terminalID string, conn *websocket.Conn) error        // ErrUnknownTerminal / ErrAlreadyAttached
  func (b *Bridge) PendingRuntime(terminalID string) (runtimeID string, ok bool)
  func (b *Bridge) TerminalCount(taskID string) int
  const MaxTerminalsPerTask = 4; MaxTerminalsPerRuntime = 12; DialBackTimeout = 10 * time.Second
  const ReasonSessionEnded, ReasonTaskNotRunning, ReasonRuntimeOffline, ReasonRuntimeNoTerminal, ReasonRuntimeUnreachable, ReasonTooManyTerminals, ReasonViewerTooSlow, ReasonClosed
  ```
  Frames to the viewer: `{"type":"viewers","count":N}` on every join/leave of the task, `{"type":"status","state":"connecting"|"attached"|"ended","reason":...}`, relayed `exit`, binary output. Frames to the daemon: binary input, relayed `resize`.

- [ ] **Step 1: Write the failing tests**

```go
package terminalbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeOpener struct {
	mu     sync.Mutex
	reason string
	opened []protocol.TerminalOpenPayload
}

func (f *fakeOpener) NotifyTerminalOpen(_ string, p protocol.TerminalOpenPayload) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, p)
	return f.reason
}

type harness struct {
	bridge *Bridge
	opener *fakeOpener
	srv    *httptest.Server
}

// newHarness serves /viewer?task=… (ServeViewer for runtime rt-1) and
// /daemon/{id} (ServeDaemon) so the test can play both ends.
func newHarness(t *testing.T) *harness {
	t.Helper()
	opener := &fakeOpener{}
	b := New(opener)
	b.dialBackTimeout = 300 * time.Millisecond
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/viewer", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b.ServeViewer(r.Context(), conn, Viewer{TaskID: r.URL.Query().Get("task"), RuntimeID: "rt-1", Session: "ctx-a-1", UserID: "u-1", Cols: 80, Rows: 24})
	})
	mux.HandleFunc("/daemon/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/daemon/")
		if _, ok := b.PendingRuntime(id); !ok {
			http.Error(w, "unknown", http.StatusNotFound)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if err := b.ServeDaemon(id, conn); err != nil {
			conn.Close()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{bridge: b, opener: opener, srv: srv}
}

func (h *harness) dial(t *testing.T, path string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.srv.URL, "http")+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readControl(t *testing.T, conn *websocket.Conn) control {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != websocket.TextMessage {
		t.Fatalf("expected a control frame, got binary %q", data)
	}
	var c control
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func waitForOpen(t *testing.T, o *fakeOpener) protocol.TerminalOpenPayload {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		o.mu.Lock()
		n := len(o.opened)
		var last protocol.TerminalOpenPayload
		if n > 0 {
			last = o.opened[n-1]
		}
		o.mu.Unlock()
		if n > 0 {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatal("opener never called")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBridgePairsViewerWithDaemonAndCopiesBothWays(t *testing.T) {
	h := newHarness(t)
	viewer := h.dial(t, "/viewer?task=task-1")
	if c := readControl(t, viewer); c.Type != "viewers" || c.Count == nil || *c.Count != 1 {
		t.Fatalf("first frame = %+v", c)
	}
	if c := readControl(t, viewer); c.Type != "status" || c.State != "connecting" {
		t.Fatalf("second frame = %+v", c)
	}
	open := waitForOpen(t, h.opener)
	if open.TaskID != "task-1" || open.Session != "ctx-a-1" || open.Cols != 80 || open.Rows != 24 || open.TerminalID == "" {
		t.Fatalf("open payload = %+v", open)
	}
	daemon := h.dial(t, "/daemon/"+open.TerminalID)
	if c := readControl(t, viewer); c.Type != "status" || c.State != "attached" {
		t.Fatalf("attached frame = %+v", c)
	}

	// viewer → daemon: bytes and resize pass through unchanged.
	if err := viewer.WriteMessage(websocket.BinaryMessage, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	if err := viewer.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":132,"rows":43}`)); err != nil {
		t.Fatal(err)
	}
	_ = daemon.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err := daemon.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "ls\r" {
		t.Fatalf("daemon got %d %q (%v)", kind, data, err)
	}
	kind, data, err = daemon.ReadMessage()
	if err != nil || kind != websocket.TextMessage || string(data) != `{"type":"resize","cols":132,"rows":43}` {
		t.Fatalf("daemon got %d %q (%v)", kind, data, err)
	}

	// daemon → viewer: output bytes.
	if err := daemon.WriteMessage(websocket.BinaryMessage, []byte("HELLO.md\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = viewer.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err = viewer.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "HELLO.md\r\n" {
		t.Fatalf("viewer got %d %q (%v)", kind, data, err)
	}

	// tmux client exit: exit frame relayed, then session_ended, then close.
	if err := daemon.WriteMessage(websocket.TextMessage, []byte(`{"type":"exit","code":0}`)); err != nil {
		t.Fatal(err)
	}
	daemon.Close()
	if c := readControl(t, viewer); c.Type != "exit" || c.Code == nil || *c.Code != 0 {
		t.Fatalf("exit relay = %+v", c)
	}
	if c := readControl(t, viewer); c.Type != "status" || c.State != "ended" || c.Reason != ReasonSessionEnded {
		t.Fatalf("ended frame = %+v", c)
	}
	if _, _, err := viewer.ReadMessage(); err == nil {
		t.Fatal("viewer socket should close after ended")
	}
	waitFor(t, func() bool { return h.bridge.TerminalCount("task-1") == 0 }, "terminal to unregister")
}

func TestBridgeReportsWhyTheDaemonCannotAttach(t *testing.T) {
	h := newHarness(t)
	h.opener.reason = ReasonRuntimeOffline
	viewer := h.dial(t, "/viewer?task=task-2")
	if c := readControl(t, viewer); c.Type != "viewers" {
		t.Fatalf("first frame = %+v", c)
	}
	if c := readControl(t, viewer); c.Type != "status" || c.State != "ended" || c.Reason != ReasonRuntimeOffline {
		t.Fatalf("ended frame = %+v", c)
	}
}

func TestBridgeTimesOutWhenTheDaemonNeverDialsBack(t *testing.T) {
	h := newHarness(t)
	viewer := h.dial(t, "/viewer?task=task-3")
	readControl(t, viewer) // viewers
	readControl(t, viewer) // connecting
	if c := readControl(t, viewer); c.Type != "status" || c.State != "ended" || c.Reason != ReasonRuntimeUnreachable {
		t.Fatalf("ended frame = %+v", c)
	}
}

func TestBridgeEnforcesThePerTaskLimit(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < MaxTerminalsPerTask; i++ {
		v := h.dial(t, "/viewer?task=task-4")
		readControl(t, v) // viewers
		readControl(t, v) // connecting — these wait for a daemon that never comes
	}
	extra := h.dial(t, "/viewer?task=task-4")
	if c := readControl(t, extra); c.Type != "status" || c.State != "ended" || c.Reason != ReasonTooManyTerminals {
		t.Fatalf("over-limit frame = %+v", c)
	}
}

func TestBridgeViewerLeavingClosesTheDaemonSide(t *testing.T) {
	h := newHarness(t)
	viewer := h.dial(t, "/viewer?task=task-5")
	readControl(t, viewer)
	readControl(t, viewer)
	open := waitForOpen(t, h.opener)
	daemon := h.dial(t, "/daemon/"+open.TerminalID)
	readControl(t, viewer) // attached
	viewer.Close()
	_ = daemon.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := daemon.ReadMessage(); err == nil {
		t.Fatal("daemon socket should close when the viewer leaves")
	}
	waitFor(t, func() bool { return h.bridge.TerminalCount("task-5") == 0 }, "terminal to unregister")
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/terminalbridge -count=1 2>&1 | tail -3'` — expected: package does not exist / undefined symbols.

- [ ] **Step 3: Implement `bridge.go`**

```go
// Package terminalbridge pairs one browser viewer of a tmux-mode task with one
// tmux client attached by the task's daemon (ContextPRO fork, spec
// 2026-09-03). It is deliberately dumb: bytes are copied, a handful of JSON
// control frames are relayed, and every pair is independent, so a viewer
// joining late gets tmux's own full repaint instead of a replayed buffer.
//
// The registry is in-memory: the daemon's dial-back must reach the same
// process as the viewer, which holds for the single-backend self-hosted stack
// this fork targets. A daemon that never dials back is reported to the viewer
// as runtime_unreachable after DialBackTimeout.
package terminalbridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	MaxTerminalsPerTask    = 4
	MaxTerminalsPerRuntime = 12
	// DialBackTimeout is how long a viewer waits for the daemon's socket.
	DialBackTimeout = 10 * time.Second
	// viewerQueue bounds frames waiting for a slow browser before it is dropped.
	viewerQueue   = 256
	daemonQueue   = 64
	writeDeadline = 10 * time.Second
)

// Status reasons sent to the viewer in {"type":"status","state":"ended"}.
const (
	ReasonSessionEnded       = "session_ended"
	ReasonTaskNotRunning     = "task_not_running"
	ReasonRuntimeOffline     = "runtime_offline"
	ReasonRuntimeNoTerminal  = "runtime_no_terminal"
	ReasonRuntimeUnreachable = "runtime_unreachable"
	ReasonTooManyTerminals   = "too_many_terminals"
	ReasonViewerTooSlow      = "viewer_too_slow"
	ReasonClosed             = "closed"
)

var (
	ErrUnknownTerminal = errors.New("terminalbridge: unknown terminal")
	ErrAlreadyAttached = errors.New("terminalbridge: terminal already has a daemon")
)

// Opener delivers the terminal_open hint. It returns "" when a capable daemon
// took it, else the status reason for the viewer (daemonws.Hub implements it).
type Opener interface {
	NotifyTerminalOpen(runtimeID string, payload protocol.TerminalOpenPayload) string
}

// Viewer describes an authenticated browser connection about to be paired.
type Viewer struct {
	TaskID    string
	RuntimeID string
	Session   string
	UserID    string
	Cols      int
	Rows      int
}

// control is every JSON text frame on either socket.
type control struct {
	Type   string `json:"type"`
	State  string `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`
	Count  *int   `json:"count,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Code   *int   `json:"code,omitempty"`
}

type terminal struct {
	id     string
	viewer Viewer
	daemon chan *websocket.Conn // buffered 1; the dial-back lands here
	out    *writer              // viewer side
}

type Bridge struct {
	opener Opener

	mu         sync.Mutex
	terminals  map[string]*terminal
	byTask     map[string]map[*terminal]bool
	perRuntime map[string]int
	// dialBackTimeout is a field so tests can shorten it.
	dialBackTimeout time.Duration
}

func New(opener Opener) *Bridge {
	return &Bridge{
		opener:          opener,
		terminals:       map[string]*terminal{},
		byTask:          map[string]map[*terminal]bool{},
		perRuntime:      map[string]int{},
		dialBackTimeout: DialBackTimeout,
	}
}

// PendingRuntime tells the daemon endpoint which runtime a terminal id belongs
// to, so it can verify the dialing daemon serves that runtime before pairing.
func (b *Bridge) PendingRuntime(terminalID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.terminals[terminalID]
	if !ok {
		return "", false
	}
	return t.viewer.RuntimeID, true
}

// TerminalCount reports how many viewers a task currently has.
func (b *Bridge) TerminalCount(taskID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.byTask[taskID])
}

// ServeViewer owns conn from here on: it opens a terminal for v, waits for the
// daemon, copies frames until either side ends, and closes conn.
func (b *Bridge) ServeViewer(ctx context.Context, conn *websocket.Conn, v Viewer) {
	out := newWriter(conn, viewerQueue)
	defer out.close()
	t, reason := b.register(v, out)
	if reason != "" {
		out.control(control{Type: "status", State: "ended", Reason: reason})
		return
	}
	defer b.unregister(t)
	payload := protocol.TerminalOpenPayload{TerminalID: t.id, TaskID: v.TaskID, Session: v.Session, Cols: v.Cols, Rows: v.Rows}
	if reason := b.opener.NotifyTerminalOpen(v.RuntimeID, payload); reason != "" {
		out.control(control{Type: "status", State: "ended", Reason: reason})
		return
	}
	out.control(control{Type: "status", State: "connecting"})
	var daemon *websocket.Conn
	select {
	case daemon = <-t.daemon:
	case <-time.After(b.dialBackTimeout):
		out.control(control{Type: "status", State: "ended", Reason: ReasonRuntimeUnreachable})
		return
	case <-ctx.Done():
		return
	}
	defer daemon.Close()
	out.control(control{Type: "status", State: "attached"})
	pump(conn, out, daemon)
}

// ServeDaemon hands the daemon's dial-back to the waiting viewer. The upgraded
// connection outlives the HTTP handler (it is hijacked), so the caller simply
// returns after this.
func (b *Bridge) ServeDaemon(terminalID string, conn *websocket.Conn) error {
	b.mu.Lock()
	t, ok := b.terminals[terminalID]
	b.mu.Unlock()
	if !ok {
		return ErrUnknownTerminal
	}
	select {
	case t.daemon <- conn:
		return nil
	default:
		return ErrAlreadyAttached
	}
}

func (b *Bridge) register(v Viewer, out *writer) (*terminal, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.byTask[v.TaskID]) >= MaxTerminalsPerTask || b.perRuntime[v.RuntimeID] >= MaxTerminalsPerRuntime {
		return nil, ReasonTooManyTerminals
	}
	t := &terminal{id: uuid.NewString(), viewer: v, daemon: make(chan *websocket.Conn, 1), out: out}
	b.terminals[t.id] = t
	if b.byTask[v.TaskID] == nil {
		b.byTask[v.TaskID] = map[*terminal]bool{}
	}
	b.byTask[v.TaskID][t] = true
	b.perRuntime[v.RuntimeID]++
	b.broadcastViewersLocked(v.TaskID)
	return t, ""
}

func (b *Bridge) unregister(t *terminal) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.terminals, t.id)
	delete(b.byTask[t.viewer.TaskID], t)
	if len(b.byTask[t.viewer.TaskID]) == 0 {
		delete(b.byTask, t.viewer.TaskID)
	}
	if b.perRuntime[t.viewer.RuntimeID]--; b.perRuntime[t.viewer.RuntimeID] <= 0 {
		delete(b.perRuntime, t.viewer.RuntimeID)
	}
	b.broadcastViewersLocked(t.viewer.TaskID)
}

// broadcastViewersLocked tells every viewer of a task how many share the
// keyboard. Safe against a closing writer: unregister removes a terminal under
// the same lock before its writer is closed (see the defer order in ServeViewer).
func (b *Bridge) broadcastViewersLocked(taskID string) {
	n := len(b.byTask[taskID])
	for t := range b.byTask[taskID] {
		count := n
		t.out.control(control{Type: "viewers", Count: &count})
	}
}

// pump copies daemon → viewer and viewer → daemon until one side ends. An exit
// frame from the daemon means the tmux client ended (session gone), so the
// viewer is told session_ended. Any other daemon disconnect is "closed": the
// viewer's client reconnects, and if the task has meanwhile ended the endpoint
// answers task_not_running. A viewer that cannot drain its queue is dropped
// with viewer_too_slow rather than stalling the PTY.
func pump(viewer *websocket.Conn, out *writer, daemon *websocket.Conn) {
	toDaemon := newWriter(daemon, daemonQueue)
	defer toDaemon.close()
	var exited, tooSlow atomic.Bool
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		for {
			kind, data, err := daemon.ReadMessage()
			if err != nil {
				return
			}
			switch kind {
			case websocket.BinaryMessage:
				if !out.send(websocket.BinaryMessage, data) {
					tooSlow.Store(true)
					return
				}
			case websocket.TextMessage:
				var c control
				if json.Unmarshal(data, &c) == nil && c.Type == "exit" {
					exited.Store(true)
					out.send(websocket.TextMessage, data)
				}
			}
		}
	}()
	viewerDone := make(chan struct{})
	go func() {
		defer close(viewerDone)
		for {
			kind, data, err := viewer.ReadMessage()
			if err != nil {
				return
			}
			switch kind {
			case websocket.BinaryMessage:
				if !toDaemon.send(websocket.BinaryMessage, data) {
					return
				}
			case websocket.TextMessage:
				var c control
				if json.Unmarshal(data, &c) == nil && c.Type == "resize" && c.Cols > 0 && c.Rows > 0 {
					toDaemon.send(websocket.TextMessage, data)
				}
			}
		}
	}()
	select {
	case <-daemonDone:
		reason := ReasonClosed
		switch {
		case exited.Load():
			reason = ReasonSessionEnded
		case tooSlow.Load():
			reason = ReasonViewerTooSlow
		}
		out.control(control{Type: "status", State: "ended", Reason: reason})
	case <-viewerDone:
	}
	daemon.Close()
	// Unblock the viewer reader without closing the viewer socket: the final
	// status frame is still in the writer's queue and is flushed by out.close().
	_ = viewer.SetReadDeadline(time.Now())
	<-daemonDone
	<-viewerDone
}

// writer is the single goroutine allowed to write to one socket (gorilla
// forbids concurrent writes). send never blocks: a full queue means the peer
// is too slow and the caller decides what to do.
type writer struct {
	conn  *websocket.Conn
	queue chan frame
	done  chan struct{}
	once  sync.Once
}

type frame struct {
	kind int
	data []byte
}

func newWriter(conn *websocket.Conn, size int) *writer {
	w := &writer{conn: conn, queue: make(chan frame, size), done: make(chan struct{})}
	go w.run()
	return w
}

func (w *writer) run() {
	defer close(w.done)
	for f := range w.queue {
		_ = w.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
		if err := w.conn.WriteMessage(f.kind, f.data); err != nil {
			for range w.queue { // drain so producers never block on a dead socket
			}
			return
		}
	}
}

func (w *writer) send(kind int, data []byte) bool {
	select {
	case w.queue <- frame{kind: kind, data: data}:
		return true
	default:
		return false
	}
}

func (w *writer) control(c control) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	w.send(websocket.TextMessage, data)
}

// close flushes queued frames, sends a close frame, and closes the socket.
func (w *writer) close() {
	w.once.Do(func() {
		close(w.queue)
		<-w.done
		_ = w.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		w.conn.Close()
	})
}
```

- [ ] **Step 4: Run the tests**

`$GOTEST 'gofmt -l ./internal/terminalbridge; go vet ./internal/terminalbridge && go test ./internal/terminalbridge -count=1 -race'` — expected `ok`. The race detector matters here: the package is all goroutines.

- [ ] **Step 5: Commit**

```bash
git add server/internal/terminalbridge
git commit -m "feat(server): terminal bridge pairing viewers with daemon tmux clients"
```

---

### Task 9: The two WebSocket endpoints and their wiring

**Files:**
- Modify: `server/internal/realtime/hub.go` (add exported `AuthenticateUpgrade` after `HandleWebSocket`)
- Create: `server/internal/handler/terminal.go`
- Modify: `server/internal/handler/handler.go` (two fields next to `Hub`/`DaemonHub`, ~line 180)
- Modify: `server/cmd/server/router.go` (after `h := handler.New(...)` at ~440; the `/ws` route at ~1336; the `/api/daemon` block at ~1436)
- Test: `server/internal/realtime/authenticate_upgrade_test.go`, `server/internal/handler/terminal_test.go`

**Interfaces:**
- Consumes: realtime `authenticateToken`, `firstMessageAuth`, `writeWSAuthFrame`, `writeWSAuthErrorAndClose`, `upgrader`, `inboundReadLimit`; handler `parseUUIDOrBadRequest`, `h.Queries.GetAgentTask`, `h.TaskService.ResolveTaskWorkspaceID`, `h.Queries.GetMemberByUserAndWorkspace`, `requireDaemonRuntimeAccess`; `util.ParseUUID` from `github.com/multica-ai/multica/server/internal/util`; `terminalbridge.Bridge`.
- Produces: `realtime.AuthenticateUpgrade(w, r, pr PATResolver) (conn *websocket.Conn, userID string, ok bool)`; `Handler.TerminalBridge *terminalbridge.Bridge`, `Handler.TerminalPATResolver realtime.PATResolver`; `GET /api/tasks/{taskId}/terminal/ws` → `TaskTerminalWebSocket` (registered outside the auth group like `/ws`); `GET /api/daemon/terminals/{terminalId}/ws` → `DaemonTerminalWebSocket` (inside `/api/daemon`, so `DaemonAuth` applies).

- [ ] **Step 1: Write the failing realtime test**

```go
package realtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/auth"
)

// AuthenticateUpgrade is the hub's auth, reused by the tmux terminal socket:
// cookie before the upgrade, or a token in the first frame after it.
func TestAuthenticateUpgradeAcceptsCookieAndFirstFrameTokens(t *testing.T) {
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "user-1"}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, uid, ok := AuthenticateUpgrade(w, r, nil)
		if !ok {
			results <- "refused"
			return
		}
		defer conn.Close()
		results <- uid
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"status","state":"attached"}`))
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Cookie path: authenticated before the upgrade.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Cookie": {auth.AuthCookieName + "=" + token}})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-results; got != "user-1" {
		t.Fatalf("cookie auth user = %q", got)
	}
	conn.Close()

	// First-frame path: upgrade first, then the auth frame, answered with auth_ack.
	conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","payload":{"token":"`+token+`"}}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(data), `"auth_ack"`) {
		t.Fatalf("first frame reply = %q (%v)", data, err)
	}
	if got := <-results; got != "user-1" {
		t.Fatalf("first-frame auth user = %q", got)
	}
	conn.Close()

	// Bad cookie: refused before the upgrade with 401.
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Cookie": {auth.AuthCookieName + "=garbage"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad cookie: err=%v resp=%v", err, resp)
	}
	if got := <-results; got != "refused" {
		t.Fatalf("bad cookie result = %q", got)
	}

	// Bad first frame: auth_error then close.
	conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, _ = conn.ReadMessage()
	if !strings.Contains(string(data), "expected auth message") {
		t.Fatalf("bad first frame reply = %q", data)
	}
	if got := <-results; got != "refused" {
		t.Fatalf("bad first frame result = %q", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/realtime -run TestAuthenticateUpgrade -count=1 2>&1 | tail -3'` — expected `undefined: AuthenticateUpgrade`.

- [ ] **Step 3: Implement the realtime helper** (hub.go, after `HandleWebSocket`)

```go
// AuthenticateUpgrade performs the hub's cookie-or-first-frame authentication
// for another WebSocket surface (the tmux live terminal, ContextPRO fork). On
// success it returns the upgraded connection and the user id; membership is
// the caller's job because the workspace is only known once the target
// resource has been loaded. On failure it has already answered the client
// (HTTP error before the upgrade, or an auth_error frame plus close after it)
// and returns ok=false.
func AuthenticateUpgrade(w http.ResponseWriter, r *http.Request, pr PATResolver) (conn *websocket.Conn, userID string, ok bool) {
	if cookie, err := r.Cookie(auth.AuthCookieName); err == nil && cookie.Value != "" {
		uid, errMsg := authenticateToken(cookie.Value, pr, r.Context())
		if errMsg != "" {
			status := http.StatusUnauthorized
			if errMsg == `{"error":"account disabled"}` {
				status = http.StatusForbidden
			}
			http.Error(w, errMsg, status)
			return nil, "", false
		}
		userID = uid
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err, "path", r.URL.Path)
		return nil, "", false
	}
	c.SetReadLimit(inboundReadLimit)
	if userID == "" {
		tokenStr, errMsg, closed := firstMessageAuth(c)
		if closed {
			return nil, "", false
		}
		if errMsg == "" {
			userID, errMsg = authenticateToken(tokenStr, pr, r.Context())
		}
		if errMsg != "" {
			writeWSAuthErrorAndClose(c, []byte(errMsg), "path", r.URL.Path)
			return nil, "", false
		}
		if !writeWSAuthFrame(c, []byte(`{"type":"auth_ack"}`), "auth_ack", "path", r.URL.Path, "user_id", userID) {
			c.Close()
			return nil, "", false
		}
	}
	return c, userID, true
}
```

Run: `$GOTEST 'gofmt -l ./internal/realtime; go vet ./internal/realtime && go test ./internal/realtime -run TestAuthenticateUpgrade -count=1'` — expected `ok`.

- [ ] **Step 4: Write the failing handler tests**

```go
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/terminalbridge"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type recordingOpener struct {
	mu     sync.Mutex
	opened []protocol.TerminalOpenPayload
	runtimes []string
}

func (o *recordingOpener) NotifyTerminalOpen(runtimeID string, p protocol.TerminalOpenPayload) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened = append(o.opened, p)
	o.runtimes = append(o.runtimes, runtimeID)
	return ""
}

// terminalTestServer mounts the viewer endpoint behind chi so {taskId} resolves,
// with a bridge whose opener records instead of reaching a daemon.
func terminalTestServer(t *testing.T) (*httptest.Server, *recordingOpener) {
	t.Helper()
	opener := &recordingOpener{}
	prevBridge, prevPR := testHandler.TerminalBridge, testHandler.TerminalPATResolver
	testHandler.TerminalBridge = terminalbridge.New(opener)
	testHandler.TerminalPATResolver = nil
	t.Cleanup(func() { testHandler.TerminalBridge, testHandler.TerminalPATResolver = prevBridge, prevPR })
	r := chi.NewRouter()
	r.Get("/api/tasks/{taskId}/terminal/ws", testHandler.TaskTerminalWebSocket)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, opener
}

func dialTerminalAs(t *testing.T, srv *httptest.Server, userID, taskID string) *websocket.Conn {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": userID}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/tasks/" + taskID + "/terminal/ws?cols=100&rows=30"
	conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Cookie": {auth.AuthCookieName + "=" + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readTerminalControl(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("frame %q: %v", data, err)
	}
	return m
}

func TestTaskTerminalWebSocketOpensATerminalForARunningTmuxTask(t *testing.T) {
	srv, opener := terminalTestServer(t)
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "terminal viewer", testutil.Cols{"priority": "medium", "number": 88301})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "tmux_session": "ctx-v-1"})

	conn := dialTerminalAs(t, srv, testUserID, taskID)
	if m := readTerminalControl(t, conn); m["type"] != "viewers" {
		t.Fatalf("first frame = %v", m)
	}
	if m := readTerminalControl(t, conn); m["type"] != "status" || m["state"] != "connecting" {
		t.Fatalf("second frame = %v", m)
	}
	opener.mu.Lock()
	defer opener.mu.Unlock()
	if len(opener.opened) != 1 || opener.runtimes[0] != runtimeID || opener.opened[0].TaskID != taskID || opener.opened[0].Session != "ctx-v-1" || opener.opened[0].Cols != 100 || opener.opened[0].Rows != 30 {
		t.Fatalf("open hint = %+v for runtimes %v", opener.opened, opener.runtimes)
	}
}

func TestTaskTerminalWebSocketRefusesOutsidersAndHeadlessTasks(t *testing.T) {
	srv, opener := terminalTestServer(t)
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "terminal refusals", testutil.Cols{"priority": "medium", "number": 88302})
	tmuxTask := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "tmux_session": "ctx-v-2"})
	headless := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running"})
	finished := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "completed", "tmux_session": "ctx-v-3"})
	outsider := dbfx.User(t, "Outsider", "outsider-terminal@example.com")

	for name, tc := range map[string]struct{ user, task string }{
		"non-member":     {outsider, tmuxTask},
		"headless task":  {testUserID, headless},
		"finished task":  {testUserID, finished},
	} {
		conn := dialTerminalAs(t, srv, tc.user, tc.task)
		if m := readTerminalControl(t, conn); m["type"] != "status" || m["state"] != "ended" || m["reason"] != terminalbridge.ReasonTaskNotRunning {
			t.Fatalf("%s: frame = %v", name, m)
		}
	}
	opener.mu.Lock()
	defer opener.mu.Unlock()
	if len(opener.opened) != 0 {
		t.Fatalf("refused viewers must not reach the daemon: %+v", opener.opened)
	}
}

func TestDaemonTerminalWebSocketRejectsUnknownTerminals(t *testing.T) {
	terminalTestServer(t)
	req := newDaemonTokenRequest("GET", "/api/daemon/terminals/nope/ws", nil, testWorkspaceID, "test-terminal-daemon")
	req = withURLParam(req, "terminalId", "nope")
	testutil.Call(t, testHandler.DaemonTerminalWebSocket, req).Want(http.StatusNotFound)
}
```

- [ ] **Step 5: Run to verify failure**

`$GOTEST 'go test ./internal/handler -run "TestTaskTerminalWebSocket|TestDaemonTerminalWebSocket" -count=1 2>&1 | tail -3'` — expected `undefined: testHandler.TaskTerminalWebSocket`.

- [ ] **Step 6: Implement the handlers and wiring**

handler.go — add next to `Hub`/`DaemonHub`:

```go
	// TerminalBridge and TerminalPATResolver serve the tmux live terminal
	// (ContextPRO fork). Set by the router after construction; nil disables
	// the endpoints with a 503.
	TerminalBridge      *terminalbridge.Bridge
	TerminalPATResolver realtime.PATResolver
```

New `terminal.go`:

```go
package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/terminalbridge"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Live terminal endpoints for tmux-mode tasks (ContextPRO fork, spec 2026-09-03).

// Daemons are not browsers; the daemon token is the credential.
var daemonTerminalUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// TaskTerminalWebSocket is the viewer side: GET /api/tasks/{taskId}/terminal/ws.
// Registered outside the auth middleware like /ws, because a desktop client
// authenticates with its first frame after the upgrade. Every refusal after the
// upgrade is the same task_not_running status: a member of another workspace
// learns nothing about whether the task exists.
func (h *Handler) TaskTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.TerminalBridge == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal bridge unavailable")
		return
	}
	taskUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "taskId"), "task_id")
	if !ok {
		return
	}
	cols, rows := terminalSize(r)
	conn, userID, ok := realtime.AuthenticateUpgrade(w, r, h.TerminalPATResolver)
	if !ok {
		return
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskUUID)
	if err != nil {
		closeTerminalWith(conn, terminalbridge.ReasonTaskNotRunning)
		return
	}
	workspaceID := h.TaskService.ResolveTaskWorkspaceID(r.Context(), task)
	if workspaceID == "" || !h.terminalViewerIsMember(r.Context(), userID, workspaceID) {
		closeTerminalWith(conn, terminalbridge.ReasonTaskNotRunning)
		return
	}
	if task.Status != "running" || !task.TmuxSession.Valid {
		closeTerminalWith(conn, terminalbridge.ReasonTaskNotRunning)
		return
	}
	taskID := uuidToString(task.ID)
	slog.Info("terminal opened", "task_id", taskID, "user_id", userID, "runtime_id", uuidToString(task.RuntimeID))
	started := time.Now()
	h.TerminalBridge.ServeViewer(r.Context(), conn, terminalbridge.Viewer{
		TaskID:    taskID,
		RuntimeID: uuidToString(task.RuntimeID),
		Session:   task.TmuxSession.String,
		UserID:    userID,
		Cols:      cols,
		Rows:      rows,
	})
	slog.Info("terminal closed", "task_id", taskID, "user_id", userID, "duration", time.Since(started).Round(time.Second))
}

// terminalViewerIsMember is the membership check the realtime hub does through
// its MembershipChecker, inlined because the workspace comes from the task.
func (h *Handler) terminalViewerIsMember(ctx context.Context, userID, workspaceID string) bool {
	uid, err := util.ParseUUID(userID)
	if err != nil {
		return false
	}
	wsid, err := util.ParseUUID(workspaceID)
	if err != nil {
		return false
	}
	_, err = h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: uid, WorkspaceID: wsid})
	return err == nil
}

// closeTerminalWith answers a viewer that was authenticated but cannot be
// paired: one status frame, then a normal close.
func closeTerminalWith(conn *websocket.Conn, reason string) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"status","state":"ended","reason":"`+reason+`"}`))
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason), time.Now().Add(time.Second))
	conn.Close()
}

// terminalSize reads the viewer's initial size from the query string; xterm.js
// knows its cols/rows before the socket opens, so the first attach is already
// the right size and there is no resize flicker.
func terminalSize(r *http.Request) (cols, rows int) {
	cols, _ = strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ = strconv.Atoi(r.URL.Query().Get("rows"))
	if cols < 20 || cols > 500 {
		cols = 120
	}
	if rows < 5 || rows > 200 {
		rows = 40
	}
	return cols, rows
}

// DaemonTerminalWebSocket is the daemon side: GET /api/daemon/terminals/{terminalId}/ws
// under DaemonAuth. The terminal id is a server-minted secret handed to one
// daemon, and that daemon must also serve the runtime the viewer's task runs on.
func (h *Handler) DaemonTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.TerminalBridge == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal bridge unavailable")
		return
	}
	terminalID := chi.URLParam(r, "terminalId")
	runtimeID, ok := h.TerminalBridge.PendingRuntime(terminalID)
	if !ok {
		writeError(w, http.StatusNotFound, "terminal not found")
		return
	}
	if _, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID); !ok {
		return
	}
	conn, err := daemonTerminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if err := h.TerminalBridge.ServeDaemon(terminalID, conn); err != nil {
		slog.Warn("terminal daemon dial-back rejected", "terminal_id", terminalID, "error", err)
		conn.Close()
	}
}
```

router.go — after `h := handler.New(...)` (line ~440) and after `pr` is defined (grep `pr :=`; move these two lines below whichever comes later):

```go
	// Live tmux terminals (ContextPRO fork): the bridge pairs browser viewers
	// with daemon dial-backs and daemonHub delivers the open hint. pr is the
	// PAT resolver the realtime hub uses, so both WebSocket surfaces accept the
	// same credentials.
	h.TerminalBridge = terminalbridge.New(daemonHub)
	h.TerminalPATResolver = pr
```

Next to the `/ws` route (~1336): `r.Get("/api/tasks/{taskId}/terminal/ws", h.TaskTerminalWebSocket)`.
Inside the `/api/daemon` block after `r.Get("/ws", h.DaemonWebSocket)`: `r.Get("/terminals/{terminalId}/ws", h.DaemonTerminalWebSocket)`.

If `daemonHub` can be nil in some server mode, guard: `if daemonHub != nil { h.TerminalBridge = terminalbridge.New(daemonHub) }` — the handler already answers 503 when the bridge is nil.

- [ ] **Step 7: Run the tests**

`$GOTEST 'gofmt -l ./internal/handler ./cmd/server ./internal/realtime; go vet ./internal/handler ./cmd/server && go test ./internal/handler -run "TestTaskTerminalWebSocket|TestDaemonTerminalWebSocket|TestReportTaskTmuxSession" -count=1'` — expected `ok`. Also `go build ./cmd/server`.

- [ ] **Step 8: Commit**

```bash
git add server/internal/realtime/hub.go server/internal/realtime/authenticate_upgrade_test.go
git commit -m "feat(realtime): export the cookie-or-first-frame WebSocket auth"
git add server/internal/handler/terminal.go server/internal/handler/terminal_test.go server/internal/handler/handler.go server/cmd/server/router.go
git commit -m "feat(server): viewer and daemon WebSocket endpoints for the tmux live terminal"
```

---

### Task 10: Close session endpoint and the issue-status hook

**Files:**
- Modify: `server/internal/handler/tmux_task.go` (append)
- Modify: `server/internal/handler/issue.go` (after the `WillEnqueueRun` block in `UpdateIssue`, ~line 3671, before `notifyParentOfChildDone` ~3679; and after the equivalent block in `BatchUpdateIssues`, ~line 4354)
- Modify: `server/cmd/server/router.go` (~line 2208, next to `/api/tasks/{taskId}/cancel`)
- Test: `server/internal/handler/tmux_task_test.go` (append)

**Interfaces:**
- Consumes: `ctxWorkspaceID(ctx)`, `h.Queries.GetAgentTaskInWorkspace`, `h.TaskService.CompleteTask(ctx, id, result, sessionID, workDir, branchName, rolloutMissing, retiredSessionID, durableWorkDir)`, `h.TaskService.CancelTaskWithReason(ctx, id, errorMessage, failureReason)`, `issuestatus.EffectiveAndName(ctx, h.Queries, wsID, status) (category, name)` (check the return order at issuestatus.go:310), `issuestatus.Done` / `issuestatus.Cancelled`, `h.Queries.ListActiveTasksByIssue`, `requestUserID(r)`.
- Produces: `POST /api/tasks/{taskId}/tmux/close` → `CloseTaskTmuxSession` (200 with `AgentTaskResponse`, 404 unknown/foreign task, 409 no live tmux session); `(*Handler).closeTmuxTasksForIssueStatus(ctx, issue db.Issue)`.

- [ ] **Step 1: Write the failing tests** (append to tmux_task_test.go; add `strings` to its imports)

```go
func TestCloseTaskTmuxSessionCompletesTheTaskAndKeepsTheSessionID(t *testing.T) {
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "tmux close from page", testutil.Cols{"priority": "medium", "number": 88203})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "tmux_session": "ctx-x-1", "session_id": "sid-close"})

	req := withChatTestWorkspaceCtx(t, newRequest("POST", "/api/tasks/"+taskID+"/tmux/close", nil))
	req = withURLParam(req, "taskId", taskID)
	var out AgentTaskResponse
	testutil.Call(t, testHandler.CloseTaskTmuxSession, req).Want(http.StatusOK).JSON(&out)
	if out.Status != "completed" {
		t.Fatalf("status = %s", out.Status)
	}
	var sessionID, status, output string
	dbfx.QueryRow(t, `SELECT session_id, status, result->>'output' FROM agent_task_queue WHERE id = $1`, taskID).Scan(&sessionID, &status, &output)
	if sessionID != "sid-close" || status != "completed" || output != "Session closed from the issue page." {
		t.Fatalf("row session_id=%q status=%q output=%q", sessionID, status, output)
	}

	// A second close is a conflict, not a silent success.
	req = withChatTestWorkspaceCtx(t, newRequest("POST", "/api/tasks/"+taskID+"/tmux/close", nil))
	req = withURLParam(req, "taskId", taskID)
	testutil.Call(t, testHandler.CloseTaskTmuxSession, req).Want(http.StatusConflict)
}

func TestCloseTaskTmuxSessionRefusesHeadlessTasks(t *testing.T) {
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "tmux close headless", testutil.Cols{"priority": "medium", "number": 88204})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running"})
	req := withChatTestWorkspaceCtx(t, newRequest("POST", "/api/tasks/"+taskID+"/tmux/close", nil))
	req = withURLParam(req, "taskId", taskID)
	testutil.Call(t, testHandler.CloseTaskTmuxSession, req).Want(http.StatusConflict)
}

// The fork's one exception to MUL-4113: a done card finishes tmux sessions and
// nothing else.
func TestMovingAnIssueToDoneClosesOnlyTmuxTasks(t *testing.T) {
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "tmux done closes session", testutil.Cols{"priority": "medium", "number": 88205, "status": "in_progress"})
	tmuxTask := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "tmux_session": "ctx-x-2", "session_id": "sid-done"})
	headless := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running"})

	req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{"status": "done"})
	req = withURLParam(req, "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)

	var tmuxStatus, sessionID, output, headlessStatus string
	dbfx.QueryRow(t, `SELECT status, session_id, result->>'output' FROM agent_task_queue WHERE id = $1`, tmuxTask).Scan(&tmuxStatus, &sessionID, &output)
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, headless).Scan(&headlessStatus)
	if tmuxStatus != "completed" || sessionID != "sid-done" || !strings.HasPrefix(output, "Session closed: issue moved to ") {
		t.Fatalf("tmux task status=%q session=%q output=%q", tmuxStatus, sessionID, output)
	}
	if headlessStatus != "running" {
		t.Fatalf("headless task must keep running (MUL-4113), got %q", headlessStatus)
	}
}

func TestMovingAnIssueToCancelledCancelsTmuxTasks(t *testing.T) {
	agentID, runtimeID := tmuxTestAgent(t)
	issueID := dbfx.Issue(t, "tmux cancelled closes session", testutil.Cols{"priority": "medium", "number": 88206, "status": "in_progress"})
	tmuxTask := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "tmux_session": "ctx-x-3"})
	req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{"status": "cancelled"})
	req = withURLParam(req, "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, tmuxTask).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("tmux task status = %q, want cancelled", status)
	}
}
```

- [ ] **Step 2: Run to verify failure**

`$GOTEST 'go test ./internal/handler -run "TestCloseTaskTmuxSession|TestMovingAnIssueTo" -count=1 2>&1 | tail -3'` — expected `undefined: testHandler.CloseTaskTmuxSession` (and, once that compiles, the two issue tests fail because the tmux task stays running).

- [ ] **Step 3: Implement** (append to tmux_task.go; add imports `context`, `fmt`, `github.com/multica-ai/multica/server/internal/issuestatus`)

```go
// tmuxCloseResult is the result body stored when the server finishes a tmux
// task: the same shape the daemon posts (output/pr_url/session_id/work_dir)
// so the UI renders it like any other run.
func tmuxCloseResult(note string) []byte {
	data, _ := json.Marshal(map[string]string{"output": note})
	return data
}

// completeTmuxTask finishes a tmux task from the server side, keeping the
// session id and work dir the daemon reported so the run stays resumable.
// CompleteAgentTask only updates status = 'running' rows, so the daemon's own
// completion report that follows the session kill is a no-op.
func (h *Handler) completeTmuxTask(ctx context.Context, task db.AgentTaskQueue, note string) (*db.AgentTaskQueue, error) {
	return h.TaskService.CompleteTask(ctx, task.ID, tmuxCloseResult(note), task.SessionID.String, task.WorkDir.String, "", false, "", "")
}

// CloseTaskTmuxSession is the issue page's "Close session":
// POST /api/tasks/{taskId}/tmux/close. Completing the task server-side is
// enough: the daemon's cancellation poll sees the terminal state and kills the
// tmux session.
func (h *Handler) CloseTaskTmuxSession(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, ctxWorkspaceID(r.Context()), "workspace id")
	if !ok {
		return
	}
	taskUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "taskId"), "task id")
	if !ok {
		return
	}
	task, err := h.Queries.GetAgentTaskInWorkspace(r.Context(), db.GetAgentTaskInWorkspaceParams{ID: taskUUID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if !task.TmuxSession.Valid || task.Status != "running" {
		writeError(w, http.StatusConflict, "task has no live tmux session")
		return
	}
	completed, err := h.completeTmuxTask(r.Context(), task, "Session closed from the issue page.")
	if err != nil {
		slog.Warn("close tmux session failed", "task_id", uuidToString(task.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to close session")
		return
	}
	slog.Info("tmux session closed by user", "task_id", uuidToString(task.ID), "user_id", requestUserID(r))
	writeJSON(w, http.StatusOK, taskToResponse(*completed, uuidToString(wsUUID)))
}

// closeTmuxTasksForIssueStatus is the fork's one exception to "status changes
// never touch running tasks" (MUL-4113 / MUL-4465): a tmux-mode task's session
// is the operator's own terminal, and they decided that moving the card to a
// done or cancelled status finishes it (spec 2026-09-03). Only tasks that
// reported a tmux session are touched; headless runs keep upstream behaviour.
func (h *Handler) closeTmuxTasksForIssueStatus(ctx context.Context, issue db.Issue) {
	category, name := issuestatus.EffectiveAndName(ctx, h.Queries, issue.WorkspaceID, issue.Status)
	if category != issuestatus.Done && category != issuestatus.Cancelled {
		return
	}
	tasks, err := h.Queries.ListActiveTasksByIssue(ctx, issue.ID)
	if err != nil {
		slog.Warn("list active tasks for tmux close failed", "issue_id", uuidToString(issue.ID), "error", err)
		return
	}
	note := fmt.Sprintf("Session closed: issue moved to %s", name)
	for _, task := range tasks {
		if !task.TmuxSession.Valid || task.Status != "running" {
			continue
		}
		if category == issuestatus.Cancelled {
			_, err = h.TaskService.CancelTaskWithReason(ctx, task.ID, note, "issue_cancelled")
		} else {
			_, err = h.completeTmuxTask(ctx, task, note)
		}
		if err != nil {
			slog.Warn("closing tmux task on issue status change failed", "task_id", uuidToString(task.ID), "error", err)
		}
	}
}
```

issue.go — in `UpdateIssue`, right after the closing brace of the `if trigger, ok := h.IssueService.WillEnqueueRun(...)` block:

```go
	// ContextPRO fork: the one status→task coupling kept, and only for tmux
	// sessions. See closeTmuxTasksForIssueStatus for the rationale next to the
	// MUL-4113 comment above.
	if statusChanged {
		h.closeTmuxTasksForIssueStatus(r.Context(), issue)
	}
```

Same four lines in `BatchUpdateIssues` after its `WillEnqueueRun` block (inside the per-issue loop, where `issue` and `statusChanged` are in scope).

router.go — next to `r.Post("/api/tasks/{taskId}/cancel", h.CancelTaskByUser)`:

```go
			r.Post("/api/tasks/{taskId}/tmux/close", h.CloseTaskTmuxSession)
```

- [ ] **Step 4: Run the tests**

`$GOTEST 'gofmt -l ./internal/handler ./cmd/server; go vet ./internal/handler ./cmd/server && go test ./internal/handler -run "Tmux|TestMovingAnIssueTo|TestUpdateIssue" -count=1'` — expected `ok`. Then the whole handler package once: `$GOTEST 'go test ./internal/handler -count=1 2>&1 | tail -3'`.

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler/tmux_task.go server/internal/handler/tmux_task_test.go server/internal/handler/issue.go server/cmd/server/router.go
git commit -m "feat(server): closing the session or finishing the card completes a tmux task"
```

---

### Task 11: Core — dependencies, task field, terminal client

**Files:**
- Modify: `pnpm-workspace.yaml` (catalog, after `'@vitejs/plugin-react'`)
- Modify: `packages/views/package.json` (dependencies, after the last `@tiptap/*` entry)
- Modify: `packages/core/package.json` (exports map)
- Modify: `packages/core/types/agent.ts` (`AgentTask`, after `branch_name`)
- Modify: `packages/core/api/schemas.ts` (`AgentTaskSchema`, after `branch_name`)
- Modify: `packages/core/api/client.ts` (after `rerunIssue`, ~line 2427)
- Create: `packages/core/api/terminal-client.ts`
- Test: `packages/core/api/schemas.test.ts` (append inside `describe("AgentTaskListSchema")`), `packages/core/api/terminal-client.test.ts`

**Interfaces:**
- Produces: `AgentTask.tmux_session?: string | null`; `api.connectionInfo(): { baseUrl: string; token: string | null }`; `api.closeTaskTmuxSession(taskId): Promise<void>`; `terminalWsUrl(apiBaseUrl, taskId, cols, rows): string`; `class TerminalClient` with `connect()`, `sendInput(data: string | Uint8Array)`, `resize(cols, rows)`, `retry()`, `close()`; types `TerminalStatus`, `TerminalEndReason`; import path `@multica/core/api/terminal-client`.

- [ ] **Step 1: Dependencies**

pnpm-workspace.yaml catalog (keep alphabetical):

```yaml
  '@xterm/addon-fit': 0.11.0
  '@xterm/xterm': 6.0.0
```

packages/views/package.json dependencies:

```json
    "@xterm/addon-fit": "catalog:",
    "@xterm/xterm": "catalog:",
```

packages/core/package.json exports, after `"./api/ws-client"`:

```json
    "./api/terminal-client": "./api/terminal-client.ts",
```

Run `pnpm install` (from the repo root; it updates `pnpm-lock.yaml`, which is committed).

- [ ] **Step 2: Write the failing tests**

schemas.test.ts — inside `describe("AgentTaskListSchema", …)`, reusing the valid task object that describe already declares (spread it as the base):

```ts
  it("keeps tmux_session when present and tolerates its absence or garbage", () => {
    const withSession = AgentTaskListSchema.parse([{ ...validTask, tmux_session: "ctx-foli-39-01a0" }]);
    expect(withSession[0]?.tmux_session).toBe("ctx-foli-39-01a0");
    expect(AgentTaskListSchema.parse([{ ...validTask, tmux_session: null }])[0]?.tmux_session).toBeNull();
    expect(AgentTaskListSchema.parse([validTask])[0]?.tmux_session).toBeUndefined();
    expect(AgentTaskListSchema.parse([{ ...validTask, tmux_session: 42 }])[0]?.tmux_session).toBeUndefined();
  });
```

(Replace `validTask` with the fixture's actual name.)

terminal-client.test.ts:

```ts
// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TerminalClient, type TerminalStatus, terminalWsUrl } from "./terminal-client";

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 3;
  readyState = FakeWebSocket.CONNECTING;
  binaryType = "blob";
  sent: (string | Uint8Array)[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  send(data: string | Uint8Array) {
    this.sent.push(data);
  }
  close() {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.();
  }
  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.();
  }
  text(obj: unknown) {
    this.onmessage?.({ data: JSON.stringify(obj) });
  }
  bytes(s: string) {
    this.onmessage?.({ data: new TextEncoder().encode(s).buffer });
  }
}

function lastSocket(): FakeWebSocket {
  const ws = FakeWebSocket.instances.at(-1);
  if (!ws) throw new Error("no socket");
  return ws;
}

describe("terminalWsUrl", () => {
  it("derives the socket URL from the API base and keeps its path prefix", () => {
    expect(terminalWsUrl("https://ctx.example.com/prefix/", "task-1", 120, 40)).toBe(
      "wss://ctx.example.com/prefix/api/tasks/task-1/terminal/ws?cols=120&rows=40",
    );
    expect(terminalWsUrl("http://localhost:8480", "task-2", 80, 24)).toBe(
      "ws://localhost:8480/api/tasks/task-2/terminal/ws?cols=80&rows=24",
    );
  });
});

describe("TerminalClient", () => {
  const statuses: TerminalStatus[] = [];
  const data: string[] = [];
  const viewers: number[] = [];
  const exits: number[] = [];
  let client: TerminalClient;

  function make(token: string | null) {
    return new TerminalClient({
      apiBaseUrl: "https://ctx.example.com",
      taskId: "task-1",
      token,
      cols: 100,
      rows: 30,
      onData: (bytes) => data.push(new TextDecoder().decode(bytes)),
      onStatus: (s) => statuses.push(s),
      onViewers: (n) => viewers.push(n),
      onExit: (code) => exits.push(code),
    });
  }

  beforeEach(() => {
    vi.useFakeTimers();
    FakeWebSocket.instances = [];
    statuses.length = data.length = viewers.length = exits.length = 0;
    vi.stubGlobal("WebSocket", FakeWebSocket);
  });
  afterEach(() => {
    client?.close();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("sends the auth frame first in token mode and nothing in cookie mode", () => {
    client = make("tok-1");
    client.connect();
    lastSocket().open();
    expect(lastSocket().sent).toEqual([JSON.stringify({ type: "auth", payload: { token: "tok-1" } })]);
    client.close();
    client = make(null);
    client.connect();
    lastSocket().open();
    expect(lastSocket().sent).toEqual([]);
  });

  it("dispatches bytes and control frames to the callbacks", () => {
    client = make(null);
    client.connect();
    const ws = lastSocket();
    ws.open();
    ws.text({ type: "viewers", count: 2 });
    ws.text({ type: "status", state: "attached" });
    ws.bytes("HELLO.md\r\n");
    ws.text({ type: "exit", code: 0 });
    ws.text({ type: "status", state: "ended", reason: "session_ended" });
    expect(viewers).toEqual([2]);
    expect(data).toEqual(["HELLO.md\r\n"]);
    expect(exits).toEqual([0]);
    expect(statuses).toEqual([{ state: "attached" }, { state: "ended", reason: "session_ended" }]);
  });

  it("encodes typed input as bytes and resize as a control frame", () => {
    client = make(null);
    client.connect();
    const ws = lastSocket();
    ws.open();
    client.sendInput("ls\r");
    client.resize(132, 43);
    expect(new TextDecoder().decode(ws.sent[0] as Uint8Array)).toBe("ls\r");
    expect(ws.sent[1]).toBe(JSON.stringify({ type: "resize", cols: 132, rows: 43 }));
  });

  it("reconnects after a non-final close and stops after a final reason", () => {
    client = make(null);
    client.connect();
    const first = lastSocket();
    first.open();
    first.text({ type: "status", state: "ended", reason: "closed" });
    first.close();
    expect(FakeWebSocket.instances).toHaveLength(1);
    vi.advanceTimersByTime(1_000);
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(lastSocket().url).toContain("cols=100&rows=30");

    const second = lastSocket();
    second.open();
    second.text({ type: "status", state: "ended", reason: "session_ended" });
    second.close();
    vi.advanceTimersByTime(60_000);
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it("close() cancels a pending reconnect", () => {
    client = make(null);
    client.connect();
    lastSocket().close();
    client.close();
    vi.advanceTimersByTime(60_000);
    expect(FakeWebSocket.instances).toHaveLength(1);
  });
});
```

- [ ] **Step 3: Run to verify failure**

`pnpm --filter @multica/core exec vitest run api/terminal-client.test.ts api/schemas.test.ts` — expected: module not found / `tmux_session` undefined.

- [ ] **Step 4: Implement**

types/agent.ts — after `branch_name`:

```ts
  /** tmux session of a tmux-mode run (ContextPRO fork). Present on finished
   *  runs too; live affordances are gated on status === "running". */
  tmux_session?: string | null;
```

schemas.ts — after `branch_name`:

```ts
  tmux_session: z.string().nullable().optional().catch(undefined),
```

client.ts — after `rerunIssue`:

```ts
  /** Connection facts a WebSocket surface (the tmux terminal) needs: the API
   *  base and the bearer token when this client runs in token mode. A null
   *  token means the HttpOnly cookie carries auth (web). */
  connectionInfo(): { baseUrl: string; token: string | null } {
    return { baseUrl: this.baseUrl, token: this.token };
  }

  /** "Close session" on a tmux-mode run: completes the task server-side; the
   *  daemon then ends the tmux session. The task list is refreshed through the
   *  usual task:* realtime events, so no body is needed here. */
  async closeTaskTmuxSession(taskId: string): Promise<void> {
    await this.fetchRaw(`/api/tasks/${taskId}/tmux/close`, { method: "POST" });
  }
```

terminal-client.ts:

```ts
// Live terminal client for tmux-mode tasks (ContextPRO fork, spec
// 2026-09-03). One WebSocket per viewer: binary frames are terminal bytes,
// text frames are small JSON control messages. Auth mirrors ws-client.ts —
// HttpOnly cookie on web, first-frame token on desktop.

export type TerminalEndReason =
  | "session_ended"
  | "task_not_running"
  | "runtime_offline"
  | "runtime_no_terminal"
  | "runtime_unreachable"
  | "too_many_terminals"
  | "viewer_too_slow"
  | "closed";

export type TerminalStatus =
  | { state: "connecting" }
  | { state: "attached" }
  | { state: "ended"; reason: TerminalEndReason };

export interface TerminalClientOptions {
  /** HTTP(S) API base, e.g. "https://ctx.example.com" or "/". */
  apiBaseUrl: string;
  taskId: string;
  /** Bearer token for first-frame auth; null means the HttpOnly cookie carries auth. */
  token: string | null;
  cols: number;
  rows: number;
  onData: (bytes: Uint8Array) => void;
  onStatus: (status: TerminalStatus) => void;
  onViewers?: (count: number) => void;
  onExit?: (code: number) => void;
}

/** Reasons after which reconnecting cannot help until the user acts (Retry). */
const FINAL_REASONS = new Set<TerminalEndReason>([
  "session_ended",
  "task_not_running",
  "runtime_offline",
  "runtime_no_terminal",
  "runtime_unreachable",
  "too_many_terminals",
]);
const RECONNECT_BASE_MS = 1_000;
const RECONNECT_MAX_MS = 10_000;

/** Builds the viewer socket URL from the API base: same origin and path
 *  prefix as /api/**, http→ws, cols/rows so the first attach is right-sized. */
export function terminalWsUrl(apiBaseUrl: string, taskId: string, cols: number, rows: number): string {
  const here = typeof globalThis.location?.href === "string" ? globalThis.location.href : "http://localhost";
  const url = new URL(apiBaseUrl, here);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  url.pathname = `${url.pathname.replace(/\/+$/, "")}/api/tasks/${encodeURIComponent(taskId)}/terminal/ws`;
  url.search = "";
  url.hash = "";
  url.searchParams.set("cols", String(cols));
  url.searchParams.set("rows", String(rows));
  return url.toString();
}

export class TerminalClient {
  private ws: WebSocket | null = null;
  private closed = false;
  private attempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private cols: number;
  private rows: number;

  constructor(private readonly options: TerminalClientOptions) {
    this.cols = options.cols;
    this.rows = options.rows;
  }

  connect(): void {
    if (this.closed) return;
    this.clearTimer();
    const ws = new WebSocket(terminalWsUrl(this.options.apiBaseUrl, this.options.taskId, this.cols, this.rows));
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    let ended: TerminalEndReason | null = null;
    ws.onopen = () => {
      if (this.options.token) {
        ws.send(JSON.stringify({ type: "auth", payload: { token: this.options.token } }));
      }
    };
    ws.onmessage = (event: MessageEvent) => {
      if (event.data instanceof ArrayBuffer) {
        this.attempt = 0; // bytes flowing means the link is healthy
        this.options.onData(new Uint8Array(event.data));
        return;
      }
      if (typeof event.data !== "string") return;
      let msg: { type?: string; state?: string; reason?: string; count?: number; code?: number };
      try {
        msg = JSON.parse(event.data);
      } catch {
        return;
      }
      switch (msg.type) {
        case "status":
          if (msg.state === "ended") {
            ended = (msg.reason as TerminalEndReason | undefined) ?? "closed";
            this.options.onStatus({ state: "ended", reason: ended });
          } else if (msg.state === "attached" || msg.state === "connecting") {
            this.options.onStatus({ state: msg.state });
          }
          break;
        case "viewers":
          if (typeof msg.count === "number") this.options.onViewers?.(msg.count);
          break;
        case "exit":
          if (typeof msg.code === "number") this.options.onExit?.(msg.code);
          break;
        default:
          break; // auth_ack and unknown frames need no action
      }
    };
    ws.onclose = () => {
      if (this.ws === ws) this.ws = null;
      if (this.closed) return;
      // A final reason means reconnecting cannot help; anything else (network
      // blip, server restart, "closed", "viewer_too_slow") is retried, and
      // tmux repaints the screen on the new attach.
      if (ended && FINAL_REASONS.has(ended)) return;
      this.scheduleReconnect();
    };
    ws.onerror = () => {
      /* onclose follows and decides */
    };
  }

  sendInput(data: string | Uint8Array): void {
    if (this.ws?.readyState !== WebSocket.OPEN) return;
    this.ws.send(typeof data === "string" ? new TextEncoder().encode(data) : data);
  }

  resize(cols: number, rows: number): void {
    this.cols = cols;
    this.rows = rows;
    if (this.ws?.readyState !== WebSocket.OPEN) return;
    this.ws.send(JSON.stringify({ type: "resize", cols, rows }));
  }

  /** Reconnect now — the Retry button after a final reason. */
  retry(): void {
    this.attempt = 0;
    this.ws?.close();
    this.connect();
  }

  close(): void {
    this.closed = true;
    this.clearTimer();
    this.ws?.close();
    this.ws = null;
  }

  private scheduleReconnect(): void {
    const delay = Math.min(RECONNECT_BASE_MS * 2 ** this.attempt, RECONNECT_MAX_MS);
    this.attempt += 1;
    this.options.onStatus({ state: "connecting" });
    this.reconnectTimer = setTimeout(() => this.connect(), delay);
  }

  private clearTimer(): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }
}
```

Note for the fake in the test: `WebSocket.OPEN` is read from the stubbed global, which is why `FakeWebSocket` declares the static constants.

- [ ] **Step 5: Run the tests**

`pnpm --filter @multica/core exec vitest run api/terminal-client.test.ts api/schemas.test.ts && pnpm --filter @multica/core typecheck` — expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add pnpm-workspace.yaml pnpm-lock.yaml packages/views/package.json packages/core/package.json packages/core/types/agent.ts packages/core/api/schemas.ts packages/core/api/schemas.test.ts packages/core/api/client.ts packages/core/api/terminal-client.ts packages/core/api/terminal-client.test.ts
git commit -m "feat(core): tmux_session on tasks and a live terminal WebSocket client"
```

---

### Task 12: The terminal panel in the issue page

**Files:**
- Create: `packages/views/issues/components/tmux-terminal-section.tsx`
- Modify: `packages/views/issues/components/issue-detail.tsx` (import near line 93; render immediately before the `<div className="my-8 border-t" />` at ~line 3265 that precedes the Activity / Comments block)
- Modify: `packages/views/locales/en/issues.json`, `zh-Hans/issues.json`, `ja/issues.json`, `ko/issues.json` (new `tmux_terminal` block; `locales/parity.test.ts` enforces all four)
- Test: `packages/views/issues/components/tmux-terminal-section.test.tsx`

**Interfaces:**
- Consumes: `api.listTasksByIssue`, `api.connectionInfo`, `api.closeTaskTmuxSession`, `TerminalClient` from `@multica/core/api/terminal-client`, `issueKeys.tasks`, `@xterm/xterm` `Terminal`, `@xterm/addon-fit` `FitAddon`, `useT("issues")`, `AlertDialog*` and `Button` from `@multica/ui`.
- Produces: `TmuxTerminalSection({ issueId })` (renders nothing without a running task with `tmux_session`), `TMUX_TERMINAL_SECTION_ID = "tmux-terminal-section"`, i18n keys under `issues.tmux_terminal.*` (listed below; Task 13 uses `open_terminal`, `resume_hint`, `resume_session`, `copy_command`, `copied`).

Before writing zh-Hans copy read `apps/docs/content/docs/developers/conventions.zh.mdx` (CLAUDE.md rule); the drafts below follow the product voice used by the existing `execution_log` block.

- [ ] **Step 1: Locale strings** — add to each `issues.json` (English shown; translate for the other three):

```json
  "tmux_terminal": {
    "title": "Terminal",
    "viewers_tooltip": "People attached to this session. Everyone can type.",
    "expand": "Expand terminal",
    "collapse": "Collapse terminal",
    "close_session": "Close session",
    "close_failed": "Couldn't close the session",
    "close_confirm_title": "Close this session?",
    "close_confirm_body": "The tmux session ends and the task is marked completed. The Claude session id stays on the run so you can resume it later.",
    "close_confirm_keep": "Keep it open",
    "close_confirm_confirm": "Close session",
    "connecting": "Connecting to the session…",
    "retry": "Reconnect",
    "reasons": {
      "session_ended": "The session has ended.",
      "task_not_running": "This task is no longer running.",
      "runtime_offline": "The runtime is offline.",
      "runtime_no_terminal": "This runtime's daemon is too old for live terminals. Update it and reconnect.",
      "runtime_unreachable": "The runtime didn't answer in time.",
      "too_many_terminals": "Too many terminals are open on this task.",
      "viewer_too_slow": "The connection couldn't keep up.",
      "closed": "The connection closed."
    },
    "open_terminal": "Terminal",
    "resume_hint": "Resume this conversation",
    "resume_session": "Resume session",
    "copy_command": "Copy command",
    "copied": "Copied"
  }
```

zh-Hans draft: `"title": "终端"`, `"viewers_tooltip": "已连接到此会话的人。所有人都可以输入。"`, `"expand": "展开终端"`, `"collapse": "收起终端"`, `"close_session": "关闭会话"`, `"close_failed": "无法关闭会话"`, `"close_confirm_title": "关闭此会话？"`, `"close_confirm_body": "tmux 会话将结束，任务标记为已完成。Claude 会话 ID 会保留在此次运行中，之后可以继续。"`, `"close_confirm_keep": "保持打开"`, `"close_confirm_confirm": "关闭会话"`, `"connecting": "正在连接会话…"`, `"retry": "重新连接"`, reasons: `"会话已结束。"`, `"此任务已不再运行。"`, `"运行环境已离线。"`, `"此运行环境的守护进程版本过旧，不支持实时终端。请更新后重新连接。"`, `"运行环境未及时响应。"`, `"此任务打开的终端过多。"`, `"连接无法跟上输出。"`, `"连接已关闭。"`, `"open_terminal": "终端"`, `"resume_hint": "继续此对话"`, `"resume_session": "继续会话"`, `"copy_command": "复制命令"`, `"copied": "已复制"`.

ja draft: `"ターミナル"`, `"このセッションに接続している人。全員が入力できます。"`, `"ターミナルを拡大"`, `"ターミナルを縮小"`, `"セッションを閉じる"`, `"セッションを閉じられませんでした"`, `"このセッションを閉じますか？"`, `"tmux セッションが終了し、タスクは完了になります。Claude のセッション ID は実行に残るので、後で再開できます。"`, `"開いたままにする"`, `"セッションを閉じる"`, `"セッションに接続中…"`, `"再接続"`, reasons: `"セッションは終了しました。"`, `"このタスクはすでに実行されていません。"`, `"ランタイムはオフラインです。"`, `"このランタイムのデーモンは古く、ライブターミナルに対応していません。更新して再接続してください。"`, `"ランタイムが時間内に応答しませんでした。"`, `"このタスクで開いているターミナルが多すぎます。"`, `"接続が出力に追いつけませんでした。"`, `"接続が閉じられました。"`, `"ターミナル"`, `"この会話を再開"`, `"セッションを再開"`, `"コマンドをコピー"`, `"コピーしました"`.

ko draft: `"터미널"`, `"이 세션에 연결된 사람들입니다. 모두 입력할 수 있습니다."`, `"터미널 확장"`, `"터미널 접기"`, `"세션 닫기"`, `"세션을 닫을 수 없습니다"`, `"이 세션을 닫을까요?"`, `"tmux 세션이 종료되고 작업은 완료로 표시됩니다. Claude 세션 ID는 실행에 남아 나중에 이어갈 수 있습니다."`, `"열어 두기"`, `"세션 닫기"`, `"세션에 연결 중…"`, `"다시 연결"`, reasons: `"세션이 종료되었습니다."`, `"이 작업은 더 이상 실행 중이 아닙니다."`, `"런타임이 오프라인입니다."`, `"이 런타임의 데몬이 오래되어 실시간 터미널을 지원하지 않습니다. 업데이트 후 다시 연결하세요."`, `"런타임이 시간 내에 응답하지 않았습니다."`, `"이 작업에 열린 터미널이 너무 많습니다."`, `"연결이 출력을 따라가지 못했습니다."`, `"연결이 닫혔습니다."`, `"터미널"`, `"이 대화 이어가기"`, `"세션 이어가기"`, `"명령 복사"`, `"복사됨"`.

Run `pnpm --filter @multica/views exec vitest run locales/parity.test.ts` — expected pass.

- [ ] **Step 2: Write the failing component test**

```tsx
// @vitest-environment jsdom

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { AgentTask } from "@multica/core/types";
import type { TerminalClientOptions } from "@multica/core/api/terminal-client";
import { issueKeys } from "@multica/core/issues/queries";
import { renderWithI18n } from "../../test/i18n";

const mocks = vi.hoisted(() => ({
  listTasksByIssue: vi.fn(),
  closeTaskTmuxSession: vi.fn(),
  clientOptions: [] as TerminalClientOptions[],
  connect: vi.fn(),
  retry: vi.fn(),
  close: vi.fn(),
  write: vi.fn(),
  sendInput: vi.fn(),
}));

vi.mock("@multica/core/api", () => ({
  api: {
    listTasksByIssue: mocks.listTasksByIssue,
    closeTaskTmuxSession: mocks.closeTaskTmuxSession,
    connectionInfo: () => ({ baseUrl: "https://ctx.test", token: null }),
  },
}));

vi.mock("@multica/core/api/terminal-client", () => ({
  TerminalClient: class {
    constructor(options: TerminalClientOptions) {
      mocks.clientOptions.push(options);
    }
    connect = mocks.connect;
    retry = mocks.retry;
    close = mocks.close;
    sendInput = mocks.sendInput;
    resize = vi.fn();
  },
}));

vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    cols = 100;
    rows = 30;
    write = mocks.write;
    open = vi.fn();
    loadAddon = vi.fn();
    dispose = vi.fn();
    onData = vi.fn(() => ({ dispose: vi.fn() }));
    onResize = vi.fn(() => ({ dispose: vi.fn() }));
  },
}));
vi.mock("@xterm/addon-fit", () => ({ FitAddon: class { fit = vi.fn(); } }));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

import { TmuxTerminalSection } from "./tmux-terminal-section";

function makeTask(overrides: Partial<AgentTask> = {}): AgentTask {
  return {
    id: "task-1", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1", status: "running", priority: 0,
    dispatched_at: null, started_at: "2026-09-03T20:00:00Z", completed_at: null, result: null, error: null,
    created_at: "2026-09-03T20:00:00Z", tmux_session: "ctx-foli-39-01a0", ...overrides,
  };
}

function renderSection(tasks: AgentTask[]) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(issueKeys.tasks("issue-1"), tasks);
  mocks.listTasksByIssue.mockResolvedValue(tasks);
  return renderWithI18n(
    <QueryClientProvider client={queryClient}>
      <TmuxTerminalSection issueId="issue-1" />
    </QueryClientProvider>,
  );
}

describe("TmuxTerminalSection", () => {
  beforeEach(() => {
    mocks.clientOptions.length = 0;
    vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); unobserve = vi.fn(); });
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("renders nothing when no running task has a tmux session", () => {
    renderSection([makeTask({ tmux_session: null }), makeTask({ id: "task-2", status: "completed" })]);
    expect(screen.queryByRole("region", { name: "Terminal" })).toBeNull();
  });

  it("attaches to the running tmux task and feeds bytes into xterm", async () => {
    renderSection([makeTask()]);
    expect(screen.getByText("ctx-foli-39-01a0")).toBeTruthy();
    await waitFor(() => expect(mocks.connect).toHaveBeenCalledTimes(1));
    const options = mocks.clientOptions[0]!;
    expect(options.taskId).toBe("task-1");
    expect(options.cols).toBe(100);
    options.onData(new TextEncoder().encode("HELLO"));
    expect(mocks.write).toHaveBeenCalledWith(new TextEncoder().encode("HELLO"));
  });

  it("explains why the terminal ended and offers a retry only when it can help", async () => {
    renderSection([makeTask()]);
    await waitFor(() => expect(mocks.clientOptions).toHaveLength(1));
    const options = mocks.clientOptions[0]!;
    options.onStatus({ state: "ended", reason: "runtime_offline" });
    expect(await screen.findByText("The runtime is offline.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Reconnect" }));
    expect(mocks.retry).toHaveBeenCalledTimes(1);
    options.onStatus({ state: "ended", reason: "session_ended" });
    expect(await screen.findByText("The session has ended.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Reconnect" })).toBeNull();
  });

  it("closes the session after confirmation", async () => {
    mocks.closeTaskTmuxSession.mockResolvedValue(undefined);
    renderSection([makeTask()]);
    fireEvent.click(screen.getByRole("button", { name: "Close session" }));
    fireEvent.click(await screen.findByRole("button", { name: "Close session", hidden: false }));
    await waitFor(() => expect(mocks.closeTaskTmuxSession).toHaveBeenCalledWith("task-1"));
  });
});
```

If the two "Close session" buttons (panel and dialog action) collide in the last test, target the dialog's with `within(screen.getByRole("alertdialog")).getByRole("button", { name: "Close session" })`.

- [ ] **Step 3: Run to verify failure**

`NODE_OPTIONS=--no-experimental-webstorage pnpm --filter @multica/views exec vitest run issues/components/tmux-terminal-section.test.tsx` — expected: cannot find module `./tmux-terminal-section`.

- [ ] **Step 4: Implement the component**

```tsx
"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, Maximize2, Minimize2, Square, Users } from "lucide-react";
import { toast } from "sonner";
import "@xterm/xterm/css/xterm.css";
import { api } from "@multica/core/api";
import { TerminalClient, type TerminalStatus } from "@multica/core/api/terminal-client";
import { issueKeys } from "@multica/core/issues/queries";
import type { AgentTask } from "@multica/core/types";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

// The live terminal of the issue's running tmux task (ContextPRO fork, spec
// 2026-09-03). Rendered in the issue's main column — the execution log lives in
// a 260px sidebar, far too narrow for a terminal. Hidden unless a running task
// has reported a tmux session; it re-attaches every time the issue is opened,
// and leaving only detaches this viewer's tmux client.
export const TMUX_TERMINAL_SECTION_ID = "tmux-terminal-section";

export function TmuxTerminalSection({ issueId }: { issueId: string }) {
  const { data: tasks = [] } = useQuery({
    queryKey: issueKeys.tasks(issueId),
    queryFn: () => api.listTasksByIssue(issueId),
  });
  const task = useMemo(
    () => tasks.find((t) => t.status === "running" && typeof t.tmux_session === "string" && t.tmux_session !== ""),
    [tasks],
  );
  if (!task) return null;
  // key: a new task gets a fresh terminal instead of reusing the old buffer.
  return <TmuxTerminalPanel key={task.id} task={task} issueId={issueId} />;
}

// xterm paints on a canvas, so design tokens must be resolved to real colours.
// The surface element carries bg-background / text-foreground / font-mono, and
// the computed style gives rgb() strings and a resolved font stack.
function readTerminalTheme(el: HTMLElement) {
  const css = getComputedStyle(el);
  return {
    fontFamily: css.fontFamily,
    theme: {
      background: css.backgroundColor,
      foreground: css.color,
      cursor: css.color,
      selectionBackground: "rgba(128, 128, 128, 0.35)",
    },
  };
}

function TmuxTerminalPanel({ task, issueId }: { task: AgentTask; issueId: string }) {
  const { t } = useT("issues");
  const queryClient = useQueryClient();
  const surfaceRef = useRef<HTMLDivElement>(null);
  const clientRef = useRef<TerminalClient | null>(null);
  const [status, setStatus] = useState<TerminalStatus>({ state: "connecting" });
  const [viewers, setViewers] = useState(1);
  const [expanded, setExpanded] = useState(false);
  const [confirmClose, setConfirmClose] = useState(false);
  const [closing, setClosing] = useState(false);

  useEffect(() => {
    const surface = surfaceRef.current;
    if (!surface) return;
    let disposed = false;
    let cleanup: (() => void) | undefined;
    // xterm.js touches `self` at import time, so it is loaded on the client
    // only, after mount.
    void Promise.all([import("@xterm/xterm"), import("@xterm/addon-fit")]).then(([{ Terminal }, { FitAddon }]) => {
      if (disposed) return;
      const look = readTerminalTheme(surface);
      const term = new Terminal({ cursorBlink: true, fontFamily: look.fontFamily, fontSize: 13, scrollback: 2000, theme: look.theme });
      const fit = new FitAddon();
      term.loadAddon(fit);
      term.open(surface);
      fit.fit();
      const { baseUrl, token } = api.connectionInfo();
      const client = new TerminalClient({
        apiBaseUrl: baseUrl,
        taskId: task.id,
        token,
        cols: term.cols,
        rows: term.rows,
        onData: (bytes) => term.write(bytes),
        onStatus: setStatus,
        onViewers: setViewers,
      });
      clientRef.current = client;
      const dataSub = term.onData((data) => client.sendInput(data));
      const resizeSub = term.onResize(({ cols, rows }) => client.resize(cols, rows));
      const observer = new ResizeObserver(() => fit.fit());
      observer.observe(surface);
      client.connect();
      cleanup = () => {
        observer.disconnect();
        dataSub.dispose();
        resizeSub.dispose();
        client.close();
        term.dispose();
        clientRef.current = null;
      };
    });
    return () => {
      disposed = true;
      cleanup?.();
    };
  }, [task.id]);

  const handleClose = async () => {
    if (closing) return;
    setClosing(true);
    try {
      await api.closeTaskTmuxSession(task.id);
      await queryClient.invalidateQueries({ queryKey: issueKeys.tasks(issueId) });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.tmux_terminal.close_failed));
      setClosing(false);
    }
  };

  // Retry helps when the daemon or network may come back; a finished session
  // or task will not.
  const canRetry = status.state === "ended" && status.reason !== "session_ended" && status.reason !== "task_not_running";

  return (
    <section
      id={TMUX_TERMINAL_SECTION_ID}
      aria-label={t(($) => $.tmux_terminal.title)}
      className={
        expanded
          ? "fixed inset-4 z-50 flex flex-col rounded-lg border bg-background shadow-lg"
          : "mt-6 flex flex-col rounded-lg border"
      }
    >
      <div className="flex items-center justify-between gap-3 px-3 py-2">
        <div className="flex min-w-0 items-center gap-2 text-caption text-muted-foreground">
          <span className="font-medium text-foreground">{t(($) => $.tmux_terminal.title)}</span>
          <code className="truncate font-mono">{task.tmux_session}</code>
          <span className="flex items-center gap-1" title={t(($) => $.tmux_terminal.viewers_tooltip)}>
            <Users className="h-3.5 w-3.5" aria-hidden />
            <span className="tabular-nums">{viewers}</span>
          </span>
        </div>
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setExpanded((v) => !v)}
            aria-label={expanded ? t(($) => $.tmux_terminal.collapse) : t(($) => $.tmux_terminal.expand)}
          >
            {expanded ? <Minimize2 className="h-3.5 w-3.5" /> : <Maximize2 className="h-3.5 w-3.5" />}
          </Button>
          <Button variant="ghost" size="sm" className="text-destructive" disabled={closing} onClick={() => setConfirmClose(true)}>
            {closing ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Square className="h-3.5 w-3.5" />}
            <span className="ml-1">{t(($) => $.tmux_terminal.close_session)}</span>
          </Button>
        </div>
      </div>
      <div className="relative min-h-0 flex-1">
        <div
          ref={surfaceRef}
          data-testid="tmux-terminal-surface"
          className={`w-full overflow-hidden rounded-b-lg bg-background font-mono text-foreground ${expanded ? "h-full" : "h-[420px]"}`}
        />
        {status.state !== "attached" && (
          <div
            role="status"
            className="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-background/80 text-body text-muted-foreground"
          >
            {status.state === "connecting" ? (
              <>
                <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                <span>{t(($) => $.tmux_terminal.connecting)}</span>
              </>
            ) : (
              <>
                <span>{t(($) => $.tmux_terminal.reasons[status.reason])}</span>
                {canRetry && (
                  <Button variant="outline" size="sm" onClick={() => clientRef.current?.retry()}>
                    {t(($) => $.tmux_terminal.retry)}
                  </Button>
                )}
              </>
            )}
          </div>
        )}
      </div>
      {confirmClose && (
        <AlertDialog open onOpenChange={setConfirmClose}>
          <AlertDialogContent onClick={(e) => e.stopPropagation()}>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.tmux_terminal.close_confirm_title)}</AlertDialogTitle>
              <AlertDialogDescription>{t(($) => $.tmux_terminal.close_confirm_body)}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>{t(($) => $.tmux_terminal.close_confirm_keep)}</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                onClick={() => {
                  setConfirmClose(false);
                  void handleClose();
                }}
              >
                {t(($) => $.tmux_terminal.close_confirm_confirm)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </section>
  );
}
```

issue-detail.tsx — add `import { TmuxTerminalSection } from "./tmux-terminal-section";` next to the `ExecutionLogSection` import, and render `<TmuxTerminalSection issueId={id} />` immediately before the `<div className="my-8 border-t" />` that precedes the `{/* Activity / Comments */}` block (same `id` prop the execution log receives).

- [ ] **Step 5: Run the tests and checks**

```bash
NODE_OPTIONS=--no-experimental-webstorage pnpm --filter @multica/views exec vitest run issues/components/tmux-terminal-section.test.tsx locales/parity.test.ts
pnpm --filter @multica/views typecheck && pnpm lint
```

Expected: pass. If lint flags the dynamic import of the CSS file, keep the static `import "@xterm/xterm/css/xterm.css"` (the katex convention) and mock it in the test as shown.

- [ ] **Step 6: Commit** (locales separately from the component, per the repo's granularity rule)

```bash
git add packages/views/locales/en/issues.json packages/views/locales/zh-Hans/issues.json packages/views/locales/ja/issues.json packages/views/locales/ko/issues.json
git commit -m "feat(views): copy for the tmux live terminal"
git add packages/views/issues/components/tmux-terminal-section.tsx packages/views/issues/components/tmux-terminal-section.test.tsx packages/views/issues/components/issue-detail.tsx
git commit -m "feat(views): live xterm.js terminal for the running tmux task on the issue page"
```

---

### Task 13: Execution log — "Terminal" chip and resume affordance

**Files:**
- Modify: `packages/views/issues/components/execution-log-section.tsx` (`ActiveTaskRow` ~307-427, `PastRow` ~428-551)
- Test: `packages/views/issues/components/execution-log-section.tmux.test.tsx` (new file, own mocks; the existing suite is left alone)

**Interfaces:**
- Consumes: `TMUX_TERMINAL_SECTION_ID` from `./tmux-terminal-section`, `api.rerunIssue(issueId, taskId)`, i18n keys `tmux_terminal.open_terminal`, `resume_hint`, `resume_session`, `copy_command`, `copied`.
- Produces: `export function sessionIdFromResult(result: unknown): string | null`; active tmux rows show a "Terminal" chip that scrolls to the panel; completed tmux rows show `claude --resume <id>` with copy and a "Resume session" button that reruns the row's agent (the claim then carries `prior_session_id`, and Task 4 makes the daemon resume it).

- [ ] **Step 1: Write the failing tests**

```tsx
// @vitest-environment jsdom

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AgentTask } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

const mocks = vi.hoisted(() => ({ rerunIssue: vi.fn(), scrollIntoView: vi.fn(), writeText: vi.fn() }));

vi.mock("@multica/core/api", () => ({
  api: { rerunIssue: mocks.rerunIssue, cancelTask: vi.fn() },
  dispatchReasonCode: () => undefined,
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span data-testid="actor-avatar" /> }));
vi.mock("../../common/task-transcript", () => ({
  TranscriptButton: ({ title }: { title?: string }) => <button type="button">{title ?? "Transcript"}</button>,
}));
vi.mock("./terminate-task-confirm-dialog", () => ({ TerminateTaskConfirmDialog: () => null }));

import { ActiveTaskRow, PastRow, sessionIdFromResult } from "./execution-log-section";

function makeTask(overrides: Partial<AgentTask> = {}): AgentTask {
  return {
    id: "task-1", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1", status: "running", priority: 0,
    dispatched_at: null, started_at: "2026-09-03T20:00:00Z", completed_at: null, result: null, error: null,
    created_at: "2026-09-03T20:00:00Z", trigger_summary: "Assigned", ...overrides,
  };
}

describe("sessionIdFromResult", () => {
  it("reads the daemon's session_id and ignores everything else", () => {
    expect(sessionIdFromResult({ session_id: "86f808da" })).toBe("86f808da");
    expect(sessionIdFromResult({ session_id: "" })).toBeNull();
    expect(sessionIdFromResult({ output: "done" })).toBeNull();
    expect(sessionIdFromResult("text")).toBeNull();
    expect(sessionIdFromResult(null)).toBeNull();
  });
});

describe("execution log tmux affordances", () => {
  beforeEach(() => {
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: mocks.writeText } });
    const section = document.createElement("section");
    section.id = "tmux-terminal-section";
    section.scrollIntoView = mocks.scrollIntoView;
    document.body.appendChild(section);
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
    document.body.innerHTML = "";
  });

  it("shows a Terminal chip on a running tmux row that scrolls to the panel", () => {
    renderWithI18n(<ActiveTaskRow task={makeTask({ tmux_session: "ctx-foli-39-01a0" })} issueId="issue-1" />);
    fireEvent.click(screen.getByRole("button", { name: "Terminal" }));
    expect(mocks.scrollIntoView).toHaveBeenCalledTimes(1);
  });

  it("shows no chip on a headless row", () => {
    renderWithI18n(<ActiveTaskRow task={makeTask()} issueId="issue-1" />);
    expect(screen.queryByRole("button", { name: "Terminal" })).toBeNull();
  });

  it("lets a completed tmux run be copied and resumed", async () => {
    mocks.rerunIssue.mockResolvedValue({});
    mocks.writeText.mockResolvedValue(undefined);
    renderWithI18n(
      <PastRow
        task={makeTask({ status: "completed", completed_at: "2026-09-03T20:10:00Z", tmux_session: "ctx-foli-39-01a0", result: { session_id: "86f808da-d52d" } })}
        issueId="issue-1"
      />,
    );
    expect(screen.getByText("claude --resume 86f808da-d52d")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Copy command" }));
    await waitFor(() => expect(mocks.writeText).toHaveBeenCalledWith("claude --resume 86f808da-d52d"));
    fireEvent.click(screen.getByRole("button", { name: "Resume session" }));
    await waitFor(() => expect(mocks.rerunIssue).toHaveBeenCalledWith("issue-1", "task-1"));
  });

  it("offers no resume on a completed headless run", () => {
    renderWithI18n(<PastRow task={makeTask({ status: "completed", completed_at: "2026-09-03T20:10:00Z", result: { session_id: "abc" } })} issueId="issue-1" />);
    expect(screen.queryByRole("button", { name: "Resume session" })).toBeNull();
  });
});
```

`PastRow` is not exported today; export it (it is already a top-level function).

- [ ] **Step 2: Run to verify failure**

`NODE_OPTIONS=--no-experimental-webstorage pnpm --filter @multica/views exec vitest run issues/components/execution-log-section.tmux.test.tsx` — expected: `sessionIdFromResult` / `PastRow` not exported.

- [ ] **Step 3: Implement**

Imports to add in execution-log-section.tsx: `Copy` and `Play` from `lucide-react`, `TMUX_TERMINAL_SECTION_ID` from `./tmux-terminal-section`.

Add the helper near the top of the file:

```tsx
// The daemon stores the Claude session id in the run result (session_id); a
// tmux run that has one can be resumed with the same conversation.
export function sessionIdFromResult(result: unknown): string | null {
  if (typeof result !== "object" || result === null) return null;
  const id = (result as { session_id?: unknown }).session_id;
  return typeof id === "string" && id !== "" ? id : null;
}

function isTmuxRun(task: AgentTask): boolean {
  return typeof task.tmux_session === "string" && task.tmux_session !== "";
}
```

`ActiveTaskRow` — after `<TaskCommentCoverage task={task} />` (always visible, unlike `RowActions` which only shows on hover):

```tsx
      {isTmuxRun(task) && (
        <button
          type="button"
          className="rounded border px-1.5 text-caption text-muted-foreground transition-colors hover:text-foreground"
          onClick={(e) => {
            e.stopPropagation();
            document.getElementById(TMUX_TERMINAL_SECTION_ID)?.scrollIntoView({ behavior: "smooth", block: "start" });
          }}
        >
          {t(($) => $.tmux_terminal.open_terminal)}
        </button>
      )}
```

`PastRow` — export it; compute `const resumeId = isTmuxRun(task) ? sessionIdFromResult(task.result) : null;` and `const resumeCommand = resumeId ? `claude --resume ${resumeId}` : null;`. Reuse `handleRetry` for resuming (it already calls `api.rerunIssue(issueId, task.id)`), so `canRetry` stays as is and the resume block renders when `resumeCommand` is set, right after `<TaskCommentCoverage task={task} />`:

```tsx
      {resumeCommand && (
        <span className="flex min-w-0 items-center gap-1 text-caption text-muted-foreground" title={t(($) => $.tmux_terminal.resume_hint)}>
          <code className="truncate font-mono">{resumeCommand}</code>
          <Tooltip>
            <TooltipTrigger
              render={
                <button
                  type="button"
                  aria-label={t(($) => $.tmux_terminal.copy_command)}
                  onClick={(e) => {
                    e.stopPropagation();
                    void navigator.clipboard.writeText(resumeCommand).then(() => toast.success(t(($) => $.tmux_terminal.copied)));
                  }}
                />
              }
              className="rounded p-1 hover:text-foreground"
            >
              <Copy className="h-3 w-3" />
            </TooltipTrigger>
            <TooltipContent>{t(($) => $.tmux_terminal.copy_command)}</TooltipContent>
          </Tooltip>
          <button
            type="button"
            className="flex items-center gap-1 rounded border px-1.5 hover:text-foreground disabled:opacity-50"
            disabled={retrying}
            onClick={(e) => {
              e.stopPropagation();
              void handleRetry();
            }}
          >
            {retrying ? <Loader2 className="h-3 w-3 animate-spin" /> : <Play className="h-3 w-3" />}
            {t(($) => $.tmux_terminal.resume_session)}
          </button>
        </span>
      )}
```

- [ ] **Step 4: Run the tests and checks**

```bash
NODE_OPTIONS=--no-experimental-webstorage pnpm --filter @multica/views exec vitest run issues/components/execution-log-section.tmux.test.tsx issues/components/execution-log-section.test.tsx
pnpm --filter @multica/views typecheck && pnpm lint
```

- [ ] **Step 5: Commit**

```bash
git add packages/views/issues/components/execution-log-section.tsx packages/views/issues/components/execution-log-section.tmux.test.tsx
git commit -m "feat(views): terminal chip and resume affordance on tmux runs"
```

---

### Task 14: Documentation and spec alignment

**Files:**
- Modify: `server/internal/service/builtin_skills/multica-runtimes-and-repos/SKILL.md` (the tmux mode section added by the 2026-09-02 plan; grep `tmux`)
- Modify: the matching `references/*-source-map.md` in the same skill directory
- Modify: `docs/superpowers/specs/2026-09-03-tmux-live-terminal-design.md`
- Modify: `docs/superpowers/specs/2026-09-02-tmux-execution-mode-design.md` (lifecycle section pointer)

- [ ] **Step 1: SKILL.md** — extend the tmux mode section with: the issue page shows a live terminal for a running tmux task and anyone with issue access can watch and type; the task completes when Claude exits, when the session is closed (any means, including "Close session" on the issue page), or when the issue moves to a done/cancelled status; the Claude session id is stored on the run (`session_id` in the result) and "Resume session" or `multica issue rerun` continues it; daemons need `local-tmux-terminal-v1` (tmux plus a PTY) for the terminal. Add the new endpoints to the source map: `POST /api/daemon/tasks/{taskId}/tmux`, `GET /api/daemon/terminals/{terminalId}/ws`, `GET /api/tasks/{taskId}/terminal/ws`, `POST /api/tasks/{taskId}/tmux/close`, with the files that implement them.

- [ ] **Step 2: Spec alignment** (the implementation deviated in four small ways; record them):
  1. Lifecycle §2: the id is reported to the server at spawn through `POST …/tmux` (so a server-side close cannot lose it); a run that fails with a non-zero exit stores no session id.
  2. Architecture §bridge: the viewer sees `session_ended` only after the daemon's `exit` frame; any other daemon disconnect is `closed` and the client reconnects, which the endpoint refuses with `task_not_running` once the task has ended.
  3. Data model: `tmux_session` is kept after the task ends (record of a tmux run) — already stated; add that the setter also writes `session_id`.
  4. Names: `SetAgentTaskTmuxSession`, `ReportTaskTmuxSession`, `CloseTaskTmuxSession`, `realtime.AuthenticateUpgrade`.
  In the 2026-09-02 spec's lifecycle section add one line: "Superseded in part by `2026-09-03-tmux-live-terminal-design.md` (completion triggers, session id, live terminal)."

- [ ] **Step 3: Commit**

```bash
git add server/internal/service/builtin_skills/multica-runtimes-and-repos docs/superpowers/specs
git commit -m "docs: live terminal, completion triggers and resume in the runtimes skill and specs"
```

---

### Task 15: Deploy and live run 5

**Files:** none in the repo. Operations only, from `/Users/bogdand/Gits/bvdr/multica`. The daemons are `com.multica.daemon.personal` / `com.multica.daemon.work` running `~/.local/bin/contextpro-multica` (memory note `macmini-multica-deployment` has the recipe and rollback).

- [ ] **Step 1: Final verification**

```bash
pnpm typecheck && pnpm lint && NODE_OPTIONS=--no-experimental-webstorage pnpm test
# Go, non-root recipe from "How to run Go checks", whole tree:
#   go test ./... -count=1
git status --porcelain   # must be empty
git push origin custom
```
Known local-only failures (memory note `multica-local-test-env-caveats`): the desktop Electron-binary test and knip's baseline. Anything else stops the deploy.

- [ ] **Step 2: Backend + web**

```bash
export VERSION="$(git describe --tags --always)" COMMIT="$(git rev-parse --short HEAD)" DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml build
docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml up -d
bash scripts/selfhost-wait.sh build
curl -s http://127.0.0.1:8480/health          # commit must equal $COMMIT
docker logs multica-backend-1 2>&1 | grep -E "445|migrat" | tail -3   # migration 445 applied
```

- [ ] **Step 3: Daemon**

```bash
docker run --rm -v "$PWD":/repo -w /repo/server -v multica-gomod:/go/pkg/mod -v multica-gocache:/root/.cache/go-build -v "$HOME/.local/bin":/out \
  -e GOOS=darwin -e GOARCH=arm64 -e CGO_ENABLED=0 golang:1.26-alpine \
  sh -c "go build -ldflags '-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE' -o /out/contextpro-multica.new ./cmd/multica"
~/.local/bin/contextpro-multica.new --version | grep "$COMMIT" && mv ~/.local/bin/contextpro-multica.new ~/.local/bin/contextpro-multica
tmux ls | grep '^ctx-' && echo "live tmux tasks exist: wait or accept the terminal drop" || true
for j in personal work; do launchctl bootout gui/$(id -u)/com.multica.daemon.$j; sleep 1; launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.multica.daemon.$j.plist; done
sleep 4; launchctl list | grep multica.daemon
grep "starting daemon" ~/.multica/daemon.log ~/.multica/profiles/work/daemon.log | tail -2   # version = $VERSION
```
A daemon restart only drops attached viewers (they reconnect); running tmux sessions are adopted.

- [ ] **Step 4: Live run 5**

1. `multica issue create --title "tmux demo run 5: live terminal" --project 1b5e6ee8-2a9b-499f-91f2-e1899eb708a8 --assignee-id 54a13083-d0b9-41d0-9804-7c43918c9a5e --description "You are running inside an interactive tmux session as a demo. In this repository, create a file named HELLO5.md containing exactly one line: hello from the live terminal. When the file exists, reply with the single word done."`
2. Open the issue in Chrome (https://multica.bogdan.is/folio/issues/FOLI-<n>). Within a few seconds the Terminal section appears under the description showing the live Claude Code screen; the execution log row shows the Terminal chip.
3. When Claude asks to create the file, press Enter inside the terminal on the page (option 1). Confirm the pane accepted it (`tmux capture-pane -p -t "=ctx-foli-<n>-xxxx:"` on the Mac Mini agrees with the browser).
4. Open the same issue in a second tab: the viewer count shows 2 in both, and both see the same screen.
5. Type `/exit` in the browser terminal: the panel shows "The session has ended.", the run completes, `multica issue runs FOLI-<n>` shows `session_id` set and `tmux_session` on the row, and the past row offers `claude --resume …` plus "Resume session".
6. Create run 6 the same way and move the card to Done while it runs: the task completes with "Session closed: issue moved to Done", `tmux ls` no longer lists the session.
7. Click "Resume session" on the completed run: a new tmux task starts, and the terminal shows Claude Code resuming the earlier conversation.
8. Rollback if needed: previous images are still tagged locally (`docker images | grep multica`), the previous daemon binary can be rebuilt from `394c1ac8a`.

- [ ] **Step 5: Record**

Update the memory notes `macmini-multica-deployment` (daemon version) and `tmux-mode-operations` (terminal on the issue page, completion triggers, resume) — ask before creating any new .md file outside the memory directory.
