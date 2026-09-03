# tmux Live Terminal — Design

Date: 2026-09-03. Builds on `2026-09-02-tmux-execution-mode-design.md` (the tmux
execution mode). Fork-only feature of ContextPRO.

## Goal

When an issue has a running tmux task, the issue page shows a real terminal
(xterm.js) attached to that tmux session. Anyone who can open the issue can watch
and type. The terminal re-attaches every time the issue is opened. The session
lives exactly as long as the task, and the Claude Code session id is stored on the
task so the conversation can be resumed later with its context.

## Non-goals

- Terminals for non-tmux runs (headless agent runs keep the transcript view).
- Mobile support.
- Per-viewer permissions beyond issue access (decided: everyone with issue access
  may type).
- Multi-instance server deployments. The bridge is in-memory; the daemon's
  terminal socket must reach the same backend instance as the viewer's. The
  self-hosted stack runs one backend. A daemon that does not dial back within the
  open timeout is reported to the viewer as unreachable.
- Scrollback replay. A fresh `tmux attach` repaints the visible screen; history
  is available inside tmux (prefix + `[`) as in any terminal.

## Lifecycle semantics (decided with the operator)

1. The tmux session stays open until the task is completed, and the task is
   completed by any of:
   - Claude Code exits (typing `/exit`, or Claude finishing and the user closing
     it). Exit code 0 completes the task; non-zero fails it (unchanged).
   - The tmux session is closed by any other means (`tmux kill-session`, typing
     `exit` in a shell that replaced Claude, the tmux server stopping, or the
     "Close session" action on the issue page). This now completes the task with
     the note "Session closed" instead of failing it with "session lost".
   - The issue is moved to a status whose category is `done` (completed) or
     `cancelled` (cancelled). The server marks the active tmux task terminal; the
     daemon's existing cancellation poll sees the terminal state and kills the
     session. This is a deliberate, tmux-only exception to upstream's rule that
     status changes never touch active tasks (see the MUL-4113 comments in
     `server/internal/handler/issue.go`); headless runs keep upstream behaviour.
2. The Claude Code session id is fixed at launch. The daemon generates a UUID and
   launches `claude --session-id <uuid>`, or `claude --resume <prior>` when the
   claim carries `prior_session_id`. The id is written to the task's `tmux.json`
   sidecar (adoption) and reported as `TaskResult.SessionID`, which the server
   already stores in `agent_task_queue.session_id`.
3. Resume: a completed tmux run shows the stored id with a copyable
   `claude --resume <id>` command and a "Resume session" action. The action uses
   the existing rerun flow (`POST /api/issues/{id}/rerun`), which creates a fresh
   tmux task; the claim's `prior_session_id` makes the daemon launch with
   `--resume`, so the new session opens with the old context. Resume is
   best-effort: Claude Code may start a copy under a new id when the original
   cannot be continued, and the daemon reports the id it launched with.
4. Opening the issue attaches a terminal; leaving the page detaches only the
   viewer's tmux client. The session is never ended by viewers coming and going.

## Architecture

Three parties, two new WebSocket endpoints, one new server→daemon frame.

```
browser ──WS /api/tasks/{taskID}/terminal/ws──▶ server ◀──WS /api/daemon/terminals/{terminalID}/ws── daemon
                                                 │   ▲                                         │
                                                 │   └── daemon:terminal_open (existing        │ tmux attach-session
                                                 │       daemon socket, one-way hint)          ▼ inside a PTY
                                                 └────── in-memory bridge, 1 viewer : 1 attach   tmux session ctx-…
```

Per viewer, one tmux client. Each browser connection gets its own PTY and its own
`tmux attach-session`, so every viewer receives tmux's full repaint on attach and
resizes follow tmux's own `window-size latest` rule (set on the session at
creation). The bridge therefore never fans out or replays bytes; it pairs one
browser socket with one daemon socket and copies frames between them.

### Protocol (`server/pkg/protocol`)

- New event `daemon:terminal_open` with payload
  `TerminalOpenPayload{TerminalID, TaskID, Session, Cols, Rows int}` sent to the
  daemon(s) serving the task's runtime through a new exported hub method
  `NotifyTerminalOpen(runtimeID string, payload TerminalOpenPayload) (delivered bool)`.
  It is a hint like `daemon:task_available`; the daemon acts by dialing back.
- New capability `local-tmux-terminal-v1` (`DaemonCapabilityLocalTmuxTerminalV1`),
  advertised when the daemon has tmux and can allocate a PTY. The browser endpoint
  refuses with reason `runtime_no_terminal` when the connected daemon lacks it.
- Terminal socket frames, identical on both endpoints:
  - Binary frame: raw bytes. Browser→server→daemon is keyboard input to the PTY;
    daemon→server→browser is PTY output. Output is chunked at 32 KB.
  - Text frame: one JSON object.
    - `{"type":"resize","cols":N,"rows":N}` browser→daemon (PTY `Setsize`).
    - `{"type":"exit","code":N}` daemon→browser when the attach process exits.
    - `{"type":"status","state":"connecting"|"attached"|"ended","reason":string}`
      server→browser. `reason` is one of `session_ended`, `task_not_running`,
      `runtime_offline`, `runtime_no_terminal`, `runtime_unreachable`,
      `too_many_terminals`, `viewer_too_slow`, `closed`.
    - `{"type":"viewers","count":N}` server→browser whenever the number of
      terminals on the task changes.
    - `{"type":"auth","payload":{"token":string}}` browser→server as the first
      frame when no auth cookie is present (desktop), mirroring the realtime hub.
      Answered with `{"type":"auth_ack"}`.

### Server: `server/internal/terminalbridge`

- `Bridge` holds pending and live terminals keyed by terminal id (UUID):
  `terminal{ID, TaskID, RuntimeID, WorkspaceID, UserID, viewer *websocket.Conn, daemon *websocket.Conn, opened time.Time}`.
- `HandleViewer(w, r)`: upgrade; authenticate (cookie via `auth.AuthCookieName`,
  else first-frame token through the exported `realtime.AuthenticateFirstMessage`
  helper extracted from the realtime hub); load the task by path id; require
  `status = running`, non-empty `tmux_session`, and membership of the caller in
  `task.workspace_id`; enforce limits (4 terminals per task, 12 per runtime);
  register a pending terminal; call `NotifyTerminalOpen`; if `delivered` is false
  send `status ended runtime_offline` and close. Otherwise send
  `status connecting`, and wait up to 10 s for the daemon socket; on timeout send
  `status ended runtime_unreachable`.
- `HandleDaemon(w, r)`: `DaemonAuth` middleware applies (Bearer `mdt_` token).
  Look up the pending terminal by path id; require that the authenticated
  daemon serves `terminal.RuntimeID`; upgrade; pair; send the viewer
  `status attached`; start two copy loops with write deadlines (10 s). Viewer
  outbound queue is 256 frames; when full the viewer is closed with
  `viewer_too_slow` and the daemon socket is closed so the attach ends.
- Closing rules: either side closing tears down the pair. The daemon closing
  after an `exit` frame yields `status ended session_ended` when the task is no
  longer running, else `closed` (the viewer client then reconnects).
- Viewer counts are recomputed per task on every pair/unpair and pushed to the
  remaining viewers of that task.
- Logging: `terminal opened` / `terminal closed` at info level with task id,
  user id, runtime id, duration and reason. No new audit table.

### Server: task state and endpoints (`server/internal/handler`)

- Migration `445_agent_task_queue_tmux_session.up.sql`:
  `ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS tmux_session TEXT;`
  (nullable, no index; lookups are by primary key). Down drops the column.
- sqlc: `SetTaskTmuxSession(id, tmux_session)`, `ClearTaskTmuxSession(id)`,
  and `tmux_session` added to the task selects used by `taskToResponse`
  (`AgentTaskResponse` gains `tmux_session: string | null`).
- `POST /api/daemon/tasks/{taskID}/tmux` body `{"session": string}` — the
  daemon reports the live session right after spawn and again on adoption.
  Guarded by the daemon task-token rules already used by progress reports.
  The value is kept after the task ends: it is the record that the run was a
  tmux run (the UI needs it for the resume affordance), and every live
  affordance is gated on `status = running` rather than on the name being set.
- `POST /api/tasks/{taskID}/tmux/close` — member action "Close session": marks
  the task `completed` with result comment `Session closed from the issue page`
  and returns the task. The daemon's cancellation poll sees the terminal state
  and kills the session; its own later report is a no-op because the task is
  already terminal. Requires workspace membership and `tmux_session` set.
- Issue status change (in `UpdateIssue`, the branch that applies a new status):
  when the new status category is `done` or `cancelled` and the issue has active
  tasks with `tmux_session` set, mark each `completed` (done) or `cancelled`
  (cancelled) with result comment `Session closed: issue moved to <status name>`.
  Tasks without `tmux_session` are untouched. Emit the existing `task:*` realtime
  events so the panel updates.

### Daemon (`server/internal/daemon`)

- `tmux_exec.go`: `NewSession` runs `set-option -t =name: window-size latest`
  after creation. New `tmuxController` method
  `Attach(ctx, name string, cols, rows int) (attachedClient, error)` that starts
  `tmux attach-session -t =name` under `creack/pty` (`pty.StartWithSize`) and
  exposes `Read`, `Write`, `Resize(cols, rows)`, `Close` (SIGHUP to the client;
  the session is unaffected), and `Wait() int`.
- `terminal.go`: handles `daemon:terminal_open` from the wakeup socket: checks
  the task is one of this daemon's live tmux tasks (state map, also populated by
  adoption), enforces 12 concurrent attaches, dials
  `/api/daemon/terminals/{terminalID}/ws` with the standard daemon headers, and
  runs the two pumps: PTY→socket (binary, 32 KB chunks) and socket→PTY (binary
  to `Write`, `resize` text frames to `Resize`). When the attach exits it sends
  `{"type":"exit","code":N}` and closes. When the socket closes it closes the
  PTY, which detaches the tmux client only.
- `tmux_runner.go`:
  - Launch args: `--session-id <uuid>` (fresh) or `--resume <prior>`; the id is
    stored in `tmuxState.ClaudeSessionID` and in `tmux.json`.
  - After spawn (and in `adoptTmuxSessions`) call `client.ReportTmuxSession`.
  - Outcome: exit code 0 → completed (unchanged); non-zero → failed (unchanged);
    missing exit code → **completed** with comment
    `Session closed before Claude Code exited.` followed by the last informative
    screen. `TaskResult.SessionID` is set in all three cases.
  - Cancellation (server-side terminal state) kills the session and reports
    nothing, as today.
- `client.go`: `tmuxAvailable() && ptyAvailable()` adds
  `local-tmux-terminal-v1`; `ptyAvailable` opens and closes one PTY once at
  startup (cached).
- Runtime capability check on the server uses the connected daemon's advertised
  capabilities (already parsed into `ClientIdentity.Capabilities`).

### Web and desktop (`packages/core`, `packages/views`)

- Dependencies (catalog): `@xterm/xterm` 6.0.0, `@xterm/addon-fit` 0.11.0.
  `packages/views` declares both; the component imports
  `@xterm/xterm/css/xterm.css` at the top, following the katex convention.
- `packages/core/api/terminal-client.ts`: `TerminalClient` built from the same
  base URL and auth mode as `ws-client.ts` (cookie on web, first-frame token on
  desktop). API: `connect(taskId, {cols, rows})`, `sendInput(bytes)`,
  `resize(cols, rows)`, `close()`, events `data`, `status`, `viewers`, `exit`.
  Reconnects with backoff 1 s → 10 s while the last status was not `ended` with
  a final reason (`session_ended`, `task_not_running`).
- `packages/core/types/agent.ts`: `AgentTask.tmux_session?: string | null`;
  `api/schemas.ts` task schema gains the optional field; malformed-response test
  updated.
- `packages/views/issues/components/tmux-terminal-section.tsx`: rendered in the
  issue detail main column, between the description and the activity/comments,
  only while an active task has `tmux_session`. Contents: header row with
  session name, viewer count, "Expand" (opens the same terminal in a full-window
  dialog) and "Close session" (confirm dialog, then
  `POST /api/tasks/{id}/tmux/close`); the xterm surface at 420 px height; an
  overlay for `connecting` and `ended` states with the reason text. Theme
  colours come from the design tokens (`--background`, `--foreground`, and the
  muted/accent tokens for the ANSI palette) read once from computed styles.
  Font: the app's monospace stack.
- `execution-log-section.tsx`: active rows with `tmux_session` show a
  "Terminal" chip that scrolls to the section; completed tmux rows (result has a
  session id and `tmux_session` set) show `claude --resume <id>` with copy and
  the "Resume session" button (rerun).
- Desktop: shared through `@multica/views`; nothing platform-specific. The
  terminal URL goes through the same origin as the API, so the Next.js proxy
  rewrite for `/api/:path*` carries the upgrade like `/ws` does today.

## Error handling

| Situation | Viewer sees | Task |
| --- | --- | --- |
| Task not running / no session | `ended · task_not_running`, panel hidden after refetch | unchanged |
| Runtime offline | `ended · runtime_offline` with the runtime name | unchanged |
| Daemon lacks capability (old build) | `ended · runtime_no_terminal`, hint to update the daemon | unchanged |
| Daemon never dials back (10 s) | `ended · runtime_unreachable`, retry button | unchanged |
| Session ends while attached | exit frame, then `ended · session_ended` | completed/failed by exit code |
| Viewer network drop | client reconnects, tmux repaints | unchanged |
| Server restart | all pairs drop, clients reconnect | unchanged |
| Daemon restart | pairs drop; adoption re-reports `tmux_session`; clients reconnect | unchanged |
| Too many terminals | `ended · too_many_terminals` | unchanged |
| Slow viewer | closed with `viewer_too_slow`, client reconnects | unchanged |

## Testing

- `server/pkg/protocol`: payload round-trip and event name test.
- `server/internal/terminalbridge`: pair lifecycle with two in-process gorilla
  connections (`httptest.Server`): bytes copy both ways, resize passthrough,
  viewer-too-slow closes both sides, daemon exit yields `session_ended`,
  open-timeout yields `runtime_unreachable`, per-task and per-runtime limits,
  viewer count frames.
- `server/internal/handler`: browser endpoint refuses non-members (close 1008),
  non-running tasks and tasks without `tmux_session`; daemon endpoint refuses a
  daemon that does not serve the runtime; `POST …/tmux` and `…/tmux/close`
  happy paths; issue moved to done completes only tmux tasks and leaves headless
  tasks running (regression for the MUL-4113 rule). Built with `dbfx` fixtures
  and `testutil.Call`.
- `server/internal/daemon`: `Attach` against a fake `tmux` script that runs
  `cat` under the PTY (echo test, resize test via `stty size`), run in the
  non-root container; `terminal_open` handling refuses unknown tasks and enforces
  the attach limit; runner tests for `--session-id`/`--resume` args, sidecar id,
  `ReportTmuxSession` call, and the missing-exit-code → completed outcome.
- TS: `terminal-client.test.ts` (node env, fake WebSocket: auth frame on desktop
  mode, binary/text dispatch, reconnect policy); `tmux-terminal-section.test.tsx`
  with `@xterm/xterm` mocked (renders header, forwards `data` to the terminal,
  shows `ended` overlay text, Close session posts and hides); schema test for
  `tmux_session`.
- Live: run 5 on the Mac Mini, watched and typed from the issue page.

## Documentation

Update `server/internal/service/builtin_skills/multica-runtimes-and-repos/SKILL.md`
and its `references/*-source-map.md` (tmux mode section: terminal on the issue
page, completion triggers, session id and resume). Update the tmux execution mode
spec's lifecycle section with a pointer here.

## Deployment

Migration 445 runs on backend start. Rebuild the backend image and the daemon
binary; the browser refuses old daemons with `runtime_no_terminal` until they are
updated.
