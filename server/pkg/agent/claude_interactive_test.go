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
	// --permission-mode is pinned to "manual" by the builder itself; the test
	// below checks that custom_args cannot change it to bypass.
	for _, forbidden := range []string{" -p ", " --print ", " --output-format ", " --input-format ", " bypassPermissions ", " --dangerously-skip-permissions ", " --disallowedTools ", " --resume "} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("interactive args contain headless flag %q: %v", strings.TrimSpace(forbidden), args)
		}
	}
	// The runtime brief tells the agent to drive ContextPRO through the multica
	// CLI. A user-level "don't ask" / auto mode blocks non-allowlisted Bash, so the
	// launch must pre-authorise that command or the very first step stalls
	// (seen on the first live tmux task, 2026-09-03).
	for _, want := range [][]string{{"--model", "claude-opus-5"}, {"--effort", "high"}, {"--mcp-config", "/tmp/task/mcp.json"}, {"--settings", "/tmp/settings.json"}, {"--add-dir", "/extra"}, {"--verbose"}, {"--allowedTools", "Bash(multica:*)"}, {"--permission-mode", "manual"}} {
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
	if !slices.Equal(none, []string{"--allowedTools", "Bash(multica:*)", "--permission-mode", "manual"}) {
		t.Fatalf("no options should still allow the multica CLI and pin manual prompting, got %v", none)
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
