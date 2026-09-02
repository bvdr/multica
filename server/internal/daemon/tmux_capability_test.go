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
