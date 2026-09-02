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
