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
