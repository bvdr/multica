package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func runtimeWithCapabilities(daemonID string, seen time.Time, caps ...string) db.AgentRuntime {
	rt := db.AgentRuntime{DaemonID: pgtype.Text{String: daemonID, Valid: daemonID != ""}}
	rt.LastSeenAt = pgtype.Timestamptz{Time: seen, Valid: true}
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
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := runtimeWithCapabilities(daemon, base)
	newer := runtimeWithCapabilities(daemon, base.AddDate(0, 0, 1), protocol.DaemonCapabilityLocalTmuxV1)
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
	if !daemonAdvertisesWorktree([]db.AgentRuntime{runtimeWithCapabilities(daemon, base, protocol.DaemonCapabilityLocalWorktreeV1)}, daemon) {
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
