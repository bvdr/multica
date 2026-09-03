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
	// The test pins PATH to this directory so no real tmux can be found; the
	// fake itself still needs touch/rm, so it restores a system PATH first.
	script := `#!/bin/sh
PATH=/usr/bin:/bin:/usr/local/bin
echo "$@" >> "$FAKE_TMUX_LOG"
case "$1" in
  new-session) shift; while [ "$1" != "-s" ]; do shift; done; touch "$FAKE_TMUX_SESSIONS/$2"; exit 0 ;;
  has-session) name="${3#=}"; [ -f "$FAKE_TMUX_SESSIONS/$name" ] && exit 0 || exit 1 ;;
  kill-session) name="${3#=}"; rm -f "$FAKE_TMUX_SESSIONS/$name"; exit 0 ;;
  pipe-pane) case "$4" in =*:) exit 0 ;; *) echo "can't find pane: $4" >&2; exit 1 ;; esac ;;
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
		"pipe-pane -o -t =ctx-x-1: cat >> '/tmp/transcript.log'",
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
