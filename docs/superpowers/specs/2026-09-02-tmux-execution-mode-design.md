# tmux execution mode for local directories

- **Status:** approved design, awaiting implementation plan
- **Date:** 2026-09-02
- **Branch:** `custom` (ContextPRO fork of multica-ai/multica)

## Goal

When an agent picks up an issue whose project (or workspace default) points at a
folder on a runtime machine, the daemon opens an **interactive Claude Code
session inside a tmux session** in that folder and hands it the task, instead
of running Claude Code headlessly. A human can `tmux attach` to watch, answer
permission prompts (moshi-hook already relays prompts from tmux panes to the
phone), and steer. The task completes when the session ends.

## Decisions made during brainstorming

| Question | Decision |
| --- | --- |
| Trigger | Third `execution_mode` on the local-directory project resource: `tmux`. Spawned on assignment like any other task. |
| Folder configuration | Workspace-level default folder plus per-project override. A project resource always wins. |
| Completion | Session lifetime: the task runs while the tmux session exists; it completes when Claude Code exits. |
| Session identity | One session per task, named `ctx-<ISSUE_IDENTIFIER>-<first 4 chars of task id>`. Parallel tasks in one folder run side by side. |
| Permissions | Normal interactive Claude Code (no `-p`, no `bypassPermissions`). |
| Approach | Daemon-native runner (approach A). |

## Non-goals (v1)

- Resuming a previous Claude Code conversation on a follow-up task (`--resume`). A follow-up starts a fresh session.
- More than one default folder per workspace.
- Any change to `in_place` or `worktree` behaviour.
- Streaming the pane transcript live into the run view. The pane is the transcript; the run view gets the tail at the end.

## Architecture

```
issue assigned ──> server claims task ──> attaches project resources
                                          (or synthesizes one from workspace default)
                                          gate: runtime must advertise local-tmux-v1
        │
        ▼
daemon runTask ──> localDirectoryAssignment (mode = tmux)
        │            skip per-path mutex, validate path as today
        ▼
   execenv.Prepare(LocalWorkDir)   writes per-task CLAUDE.md brief, MCP config, sidecars
        │
        ▼
   write <envRoot>/tasks/<taskID>/{prompt.md, run.sh, tmux.json}
   tmux new-session -d -s <name> -c <folder> -- sh run.sh
   tmux pipe-pane -o -t <name> 'cat >> transcript.log'
        │
        ▼
   StartTask + ReportProgress("Interactive session on <device>: tmux attach -t <name>")
   ReportTaskMessages(same text)
        │
        ▼
   poll tmux has-session every 2s (heartbeat as today)
        │
        ├─ session gone, exit-code == 0 ──> CompleteTask(output = ANSI-stripped tail of transcript)
        ├─ session gone, exit-code != 0 ──> FailTask(tail)
        ├─ session gone, no exit-code    ──> FailTask("session lost")
        └─ task cancelled from UI        ──> tmux kill-session, existing cancel path
```

## Data model and API

### Execution mode

`execution_mode` on the `local_directory` resource ref gains the value `tmux`.
The JSONB shape is unchanged: `{local_path, daemon_id, label?, execution_mode?}`.
No migration; the field is free-form JSON validated in code.

### Workspace default folder

New nullable JSONB column `workspace.default_local_directory`, holding exactly
the same ref shape as a project resource. This follows the existing
`workspace.repos` and `workspace.mcp_config` JSONB columns: no foreign key,
no cascade, no index (the column is read by primary key only).

- Migration `server/migrations/444_workspace_default_local_directory.up.sql`:
  `ALTER TABLE workspace ADD COLUMN IF NOT EXISTS default_local_directory JSONB;` (plus the matching down file).
- sqlc: extend the workspace queries in `server/pkg/db/queries/workspace.sql` so `GetWorkspace`
  returns the column and `UpdateWorkspace` can set it; run `make sqlc`.
- API: `GET /workspaces/{slug}` (handler `GetWorkspace`) returns
  `default_local_directory` (nullable object). `UpdateWorkspace` accepts it;
  the ref is validated with the same rules as a project resource
  (`validateLocalDirectoryRef`), including the mode gate below. Sending
  `null` clears it.
- Frontend: `packages/core/types/workspace.ts` gains
  `default_local_directory: LocalDirectoryRef | null`; the zod workspace schema
  in `packages/core/api` parses it with `parseWithFallback`, defaulting to
  `null` on malformed input. Malformed-response test required.

### Resolution at claim time

In the claim handler (server/internal/handler/daemon.go, where
`resp.ProjectResources` is assembled): if the task has a project and that
project has no `local_directory` resource, and the workspace has a
`default_local_directory`, append one synthesized `ProjectResourceData`
with `resource_type = local_directory`, `id = "workspace-default"`, and the
default's ref. Leader tasks are unaffected (the daemon already ignores
assignments for them). The daemon path is otherwise identical for both sources.

## Server changes

- `server/pkg/protocol/messages.go`: add
  `DaemonCapabilityLocalTmuxV1 = "local-tmux-v1"` with a comment mirroring the
  worktree rationale (a daemon that implements the mode says so; version
  strings cannot answer it).
- `server/internal/handler/project_resource.go`: add
  `localDirectoryModeTmux = "tmux"`; accept it in mode validation; extend the
  save-time gate so choosing `tmux` for a daemon whose newest runtime row lacks
  `local-tmux-v1` (via `runtimeHasCapability`) fails with a message naming the
  fix ("install tmux on that machine and restart the ContextPRO runtime, or
  pick another mode"). The same validation runs for the workspace default.
- `server/internal/handler/daemon.go`: generalise `worktreeClaimBlockReason`
  into a per-mode check (`localDirectoryClaimBlockReason`) that also cancels a
  `tmux` task when the claiming daemon did not send `local-tmux-v1` in
  `X-Client-Capabilities`. Fail closed, exactly like worktree: never run in
  place instead.
- Config endpoint / API docs: none beyond the workspace field.

## Daemon changes (server/internal/daemon)

- **Capability.** `daemonClientCapabilities()` includes `local-tmux-v1` only when
  `exec.LookPath("tmux")` succeeds at startup. The lookup result is cached on
  the daemon and logged once at startup ("tmux found at ...", or "tmux not
  found: interactive mode unavailable").
- **Assignment.** `local_directory.go`: add `localDirectoryModeTmux`; accept it in
  `ValidateExecutionMode`; add `UsesTmux()`. `localDirectoryLockExempt` returns
  true for tmux assignments (parallel sessions in one folder are the chosen
  behaviour; comment at the exemption point says so). GC-meta stamping and the
  env-root exemption behave as for `in_place` (the user's folder is never
  cleaned).
- **Runner.** New file `tmux_runner.go`. `runTask` branches to it right after
  the assignment is validated and `execenv.Prepare` has run with
  `LocalWorkDir` set (so the per-task CLAUDE.md brief, MCP config and sidecars
  exist exactly as for `in_place`). The runner:
  1. Builds the prompt with `BuildPrompt(task, provider)` and writes it to
     `<envRoot>/tasks/<taskID>/prompt.md`.
  2. Writes `run.sh`: `cd` to the folder; `exec` is not used so the exit code
     can be captured; runs `claude` with `--model` and `--mcp-config` (plus
     `--strict-mcp-config` under the same condition as headless) when the agent
     configures them, and the prompt as the positional argument read from the
     file (`"$(cat prompt.md)"`), which avoids tmux command quoting; writes
     `$?` to `exit-code`. No `-p`, no `--output-format`, no
     `--permission-mode`, no `--disallowedTools AskUserQuestion`.
  3. Session name: `ctx-<IssueIdentifier>-<taskID[:4]>`, lower-cased, with any
     character outside `[a-z0-9-]` replaced by `-` (tmux rejects `.` and `:`).
     If a session with that name exists, append `-2`, `-3`, ...
  4. `tmux new-session -d -s <name> -c <folder> -- sh <run.sh>`, then
     `tmux pipe-pane -o -t <name> 'cat >> <transcript.log>'`.
  5. Writes `tmux.json` (`session`, `task_id`, `issue_id`, paths, started_at)
     next to the prompt for restart adoption.
  6. `StartTask`; `ReportProgress(summary)` and `ReportTaskMessages` with
     "Interactive session on <DeviceName>: `tmux attach -t <name>`".
  7. Watch loop: every 2s `tmux has-session -t <name>`; heartbeat as other
     tasks. On exit: read `exit-code`; `0` completes with output = last 200
     lines of the transcript, ANSI escape sequences stripped, prefixed by one
     line naming the session; non-zero fails with the same tail and
     `failure_reason = "interactive_session_exit"`; missing file fails with
     "interactive session ended without an exit code (session lost)".
  8. Cancel: when the task context is cancelled, `tmux kill-session -t <name>`
     then the existing cancel handling. Files under `<envRoot>/tasks/<taskID>`
     are removed after the final report; the user's folder is untouched apart
     from the sidecars the manifest already tracks and rolls back.
- **Restart adoption.** On startup, for every `tmux.json` under
  `<envRoot>/tasks/`: if the session exists, resume the watch loop for that
  task; if not, report from `exit-code` as above, or fail with "session lost".
  Reports go through the same `CompleteTask`/`FailTask` calls; a task the server
  already closed is logged and its files removed.
- **Binary resolution.** `tmux` and `claude` are resolved with `exec.LookPath`
  on the daemon's PATH, the same PATH the launchd plists set. Tests never resolve
  real binaries (see Testing).

## Frontend changes (packages/views, packages/core)

- `projects/components/local-directory-mode-dialog.tsx`: third card
  "Interactive terminal (tmux)" with description "Opens Claude Code in a tmux
  session in this folder. You attach to watch and approve; the task ends when
  the session ends." Disabled with a reason when the selected runtime lacks
  `local-tmux-v1`, using the same `unavailableReason` mechanism as worktree.
- `projects/components/project-resources-section.tsx`: show the mode label for
  tmux resources.
- `settings/components/repositories-tab.tsx`: new "Default local directory"
  block: runtime picker (registered runtimes with the capability flagged),
  absolute path input, mode picker reusing the dialog, clear button. Saves via
  the workspace update mutation; optimistic updates are not used (settings save
  awaits the server).
- Locale keys in all four `locales/*/{resources,settings}.json`; brand-neutral
  copy (the product name is already ContextPRO).
- Run view: no new component. The attach command shows in the progress line and
  first run message; the transcript tail shows in the output area.

## Built-in skills

`server/internal/service/builtin_skills/multica-runtimes-and-repos/SKILL.md`
and its source map: one short paragraph explaining the tmux mode, the attach
command, and that completion happens when the session ends.

## Error handling

| Situation | Behaviour |
| --- | --- |
| tmux missing on the runtime | Capability absent; UI card disabled; save rejected; claim of an existing tmux resource cancelled with a reason. |
| Folder invalid / missing | Same `validateLocalPath` errors as `in_place`, before any session is spawned. |
| `tmux new-session` fails | FailTask with tmux stderr; nothing to clean but the task dir. |
| Claude Code not on PATH | `run.sh` exits 127; FailTask with "claude not found on the runtime PATH". |
| Daemon restarts mid-session | Adopted on startup; no duplicate session, no duplicate report. |
| Session killed by hand | Treated as exit without an exit code unless Claude wrote one first: "session lost" failure. |
| Two tasks, same folder | Two sessions; no mutex; edits may interleave (chosen). |

## Testing

- Server (Go, container with throwaway Postgres): mode validation accepts
  `tmux`; save-time gate with and without the capability; claim-time cancel
  without the capability; synthesized default present / absent / overridden by
  a project resource; workspace GET/UPDATE round-trip and `null` clearing.
- Daemon (Go): fake `tmux` and fake `claude` scripts created by the test and
  put first on PATH (never the real CLIs). Cover: capability present only when
  the fake tmux resolves; session name sanitising and collision suffix; run.sh
  contents (no headless flags, model/MCP flags when configured); pipe-pane
  invocation; exit-code mapping (0, non-zero, missing); cancel kills the
  session; restart adoption for live, finished and lost sessions; lock
  exemption.
- Frontend (Vitest, `NODE_OPTIONS=--no-experimental-webstorage` locally): mode
  card renders and disables with reason; settings block save/clear; workspace
  schema accepts, defaults on malformed input.
- Not covered by e2e; the flow needs a real tmux and Claude Code.

## Rollout on the Mac Mini

1. Deploy backend and web as usual (build override compose, `up -d`). With the
   old daemon still running, nothing changes for users: no runtime advertises
   the capability yet, so the mode cannot be selected.
2. Cross-compile the fork CLI from the golang container:
   `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-X main.version=<tag> -X main.commit=<sha> -X main.date=<date>" -o contextpro-multica ./cmd/multica`
   (no cgo anywhere in the tree). Install to `~/.local/bin/contextpro-multica`.
3. Point both launchd plists (`com.multica.daemon.personal`, `com.multica.daemon.work`)
   at that binary and reload them. Self-hosted daemons have auto-update off by
   default (MUL-2381) and the fork version is not an upstream release, so
   upstream cannot replace the binary. `MULTICA_DAEMON_AUTO_RELOAD` may stay on;
   it only restarts when the binary on disk changes.
4. Set the workspace default folder in Settings, or a project resource, pick
   "Interactive terminal (tmux)", assign an issue, and `tmux attach`.

## Open points

None blocking. Two knobs deliberately fixed in v1 and easy to expose later:
the transcript tail length (200 lines) and the poll interval (2s).
