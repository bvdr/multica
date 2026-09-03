package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
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
	env := map[string]string{"MULTICA_TOKEN": "mat_secret", "MULTICA_DAEMON_PORT": "8481", "PATH": "/opt/homebrew/bin:/usr/bin", "bad key": "x"}
	script := renderTmuxRunScript("/Users/dev/app", "/usr/local/bin/claude", []string{"--model", "claude-opus-5"}, "/root/.tmux-tasks/t1/prompt.md", "/root/.tmux-tasks/t1/exit-code", env)
	for _, want := range []string{
		"#!/bin/sh",
		// The per-task environment the headless launch injects (task token,
		// daemon port, private config root, PATH) must reach the interactive
		// session too, or the multica CLI inside it refuses to authenticate.
		"export MULTICA_DAEMON_PORT='8481'",
		"export MULTICA_TOKEN='mat_secret'",
		"export PATH='/opt/homebrew/bin:/usr/bin'",
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
	for _, forbidden := range []string{" -p ", "--output-format", "bypassPermissions", "bad key"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("script contains %q", forbidden)
		}
	}
	// Exports come before the cd so a relative PATH entry cannot change meaning.
	if strings.Index(script, "export MULTICA_TOKEN") > strings.Index(script, "cd '/Users/dev/app'") {
		t.Error("environment exports must precede the cd")
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

// fakeTmux is an in-memory controller: sessions are alive until end() is
// called, and every call is recorded for assertions.
type fakeTmux struct {
	mu      sync.Mutex
	alive   map[string]bool
	newArgs [][]string
	piped   map[string]string
	killed  []string
	screen  map[string]string // rendered pane text returned by CapturePane
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{alive: map[string]bool{}, piped: map[string]string{}, screen: map[string]string{}}
}
func (f *fakeTmux) CapturePane(_ context.Context, name string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.alive[name] {
		return "", errors.New("can't find pane")
	}
	return f.screen[name], nil
}
func (f *fakeTmux) setScreen(name, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.screen[name] = text
}
func (f *fakeTmux) NewSession(_ context.Context, name, folder string, command []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[name] = true
	f.newArgs = append(f.newArgs, append([]string{name, folder}, command...))
	return nil
}
func (f *fakeTmux) PipePane(_ context.Context, name, transcript string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.piped[name] = transcript
	return nil
}
func (f *fakeTmux) HasSession(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[name], nil
}
func (f *fakeTmux) KillSession(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.alive, name)
	f.killed = append(f.killed, name)
	return nil
}
func (f *fakeTmux) end(name string) { f.mu.Lock(); defer f.mu.Unlock(); delete(f.alive, name) }
func (f *fakeTmux) firstAlive() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n := range f.alive {
		return n
	}
	return ""
}

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
		result, runErr = d.runTmuxTask(context.Background(), task, env, assignment, "/opt/fake/claude", agent.ExecOptions{Model: "claude-opus-5"}, map[string]string{"MULTICA_TOKEN": "mat_demo"}, "Do the thing", slog.Default())
	}()

	// Wait for the session to exist, then simulate Claude finishing.
	var name string
	for i := 0; i < 200 && name == ""; i++ {
		name = ctl.firstAlive()
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
	// The interactive launch always pre-authorises the multica CLI first.
	if !strings.Contains(string(script), "export MULTICA_TOKEN='mat_demo'") {
		t.Fatalf("run.sh does not export the task environment:\n%s", script)
	}
	if !strings.Contains(string(script), "'/opt/fake/claude' '--allowedTools' 'Bash(multica:*)' '--permission-mode' 'manual' '--model' 'claude-opus-5'") {
		t.Fatalf("run.sh does not launch claude interactively:\n%s", script)
	}
	ctl.mu.Lock()
	piped := ctl.piped[name]
	ctl.mu.Unlock()
	if piped != filepath.Join(taskDir, "transcript.log") {
		t.Fatalf("pipe-pane transcript = %q", piped)
	}
	os.WriteFile(filepath.Join(taskDir, "transcript.log"), []byte("\x1b[1mraw tui bytes\x1b[0m\n"), 0o600)
	// While the session is alive the watcher snapshots the rendered screen; the
	// result must show that (what a human saw), not the raw TUI transcript.
	ctl.setScreen(name, "⏺ Wrote HELLO.md\n\ndone\n\n\n")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(taskDir, "screen.txt")); err == nil && strings.Contains(string(b), "done") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never snapshotted the rendered screen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.WriteFile(filepath.Join(taskDir, "exit-code"), []byte("0\n"), 0o600)
	ctl.end(name)
	<-done

	if runErr != nil {
		t.Fatalf("runTmuxTask: %v", runErr)
	}
	if result.Status != "completed" || !strings.Contains(result.Comment, "⏺ Wrote HELLO.md\n\ndone") || strings.Contains(result.Comment, "raw tui") || result.WorkDir != folder {
		t.Fatalf("result = %+v", result)
	}
	if strings.HasSuffix(result.Comment, "\n\n\n") {
		t.Fatalf("trailing blank screen lines should be trimmed: %q", result.Comment)
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
	// No screen snapshot exists for a session that was already gone: the raw
	// transcript tail stays the fallback so the failure still shows something.
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
	ctl.mu.Lock()
	killed := append([]string(nil), ctl.killed...)
	ctl.mu.Unlock()
	if len(killed) != 1 || killed[0] != "ctx-a-1" {
		t.Fatalf("cancelled task should kill its session, killed=%v", killed)
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
	ctl2.mu.Lock()
	defer ctl2.mu.Unlock()
	if len(ctl2.killed) != 0 || !ctl2.alive["ctx-b-1"] {
		t.Fatalf("daemon shutdown must leave the session alive, killed=%v alive=%v", ctl2.killed, ctl2.alive)
	}
}

func TestAdoptTmuxSessionsResumesLiveAndSettlesDead(t *testing.T) {
	orig := tmuxPollInterval
	tmuxPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tmuxPollInterval = orig })

	ctl := newFakeTmux()
	d := newTmuxTestDaemon(t, ctl)
	var mu sync.Mutex
	settled := map[string]string{} // task id -> "completed" | "failed: ..."
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

	waitFor := func(cond func() bool, what string) {
		deadline := time.After(2 * time.Second)
		for !cond() {
			select {
			case <-deadline:
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("%s; settled=%v", what, settled)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return len(settled) == 3 }, "dead sessions not settled in time")
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
	waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return settled["live"] == "completed" }, "live session did not settle after it ended")
	if _, err := os.Stat(filepath.Join(d.cfg.WorkspacesRoot, tmuxTasksDirName, "corrupt")); !os.IsNotExist(err) {
		t.Fatal("corrupt state dir should be removed")
	}
}

// When Claude Code exits it leaves the alternate screen and prints only a
// goodbye ("Resume this session with …"), so the very last snapshot says
// nothing about the work. The output must fall back to the previous snapshot
// and append the short final one.
func TestTmuxOutputTextPrefersTheLastInformativeScreen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := tmuxState{ScreenPath: filepath.Join(dir, "screen.txt"), TranscriptPath: filepath.Join(dir, "transcript.log")}
	os.WriteFile(filepath.Join(dir, "screen.prev.txt"), []byte("⏺ Created HELLO.md\n⏺ done\n\n"), 0o600)
	os.WriteFile(st.ScreenPath, []byte("Resume this session with:\nclaude --resume abc\n\n\n"), 0o600)
	got := tmuxOutputText(st)
	if !strings.HasPrefix(got, "⏺ Created HELLO.md\n⏺ done") || !strings.Contains(got, "claude --resume abc") {
		t.Fatalf("output should combine the last informative screen with the goodbye, got %q", got)
	}
	// A rich final screen is used as is.
	os.WriteFile(st.ScreenPath, []byte("line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\n"), 0o600)
	if got := tmuxOutputText(st); got != "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\n" {
		t.Fatalf("rich final screen changed: %q", got)
	}
}

func TestWatchTmuxSessionKeepsThePreviousSnapshot(t *testing.T) {
	orig := tmuxPollInterval
	tmuxPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tmuxPollInterval = orig })
	ctl := newFakeTmux()
	d := newTmuxTestDaemon(t, ctl)
	dir := tmuxTaskDir(d.cfg.WorkspacesRoot, "snap")
	os.MkdirAll(dir, 0o700)
	st := tmuxState{Session: "ctx-snap", TaskID: "snap", ScreenPath: filepath.Join(dir, "screen.txt"), ExitCodePath: filepath.Join(dir, "exit-code"), TranscriptPath: filepath.Join(dir, "transcript.log")}
	ctl.alive["ctx-snap"] = true
	ctl.setScreen("ctx-snap", "working: step one\nworking: step two\nworking: step three\nstep four\nstep five\nstep six\n")
	done := make(chan struct{})
	var res TaskResult
	var err error
	go func() {
		defer close(done)
		res, err = d.watchTmuxSession(context.Background(), ctl, st, slog.Default())
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if b, _ := os.ReadFile(st.ScreenPath); strings.Contains(string(b), "step six") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first snapshot never written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctl.setScreen("ctx-snap", "Resume this session with:\nclaude --resume xyz\n")
	deadline = time.Now().Add(2 * time.Second)
	for {
		if b, _ := os.ReadFile(st.ScreenPath); strings.Contains(string(b), "resume xyz") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second snapshot never written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.WriteFile(st.ExitCodePath, []byte("0"), 0o600)
	ctl.end("ctx-snap")
	<-done
	if err != nil || !strings.HasPrefix(res.Comment, "Interactive session ctx-snap finished.\n\nworking: step one") || !strings.Contains(res.Comment, "claude --resume xyz") {
		t.Fatalf("result = %+v (%v)", res, err)
	}
}
