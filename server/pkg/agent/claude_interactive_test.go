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
	for _, forbidden := range []string{" -p ", " --print ", " --output-format ", " --input-format ", " --permission-mode ", " --dangerously-skip-permissions ", " --disallowedTools ", " --resume "} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("interactive args contain headless flag %q: %v", strings.TrimSpace(forbidden), args)
		}
	}
	for _, want := range [][]string{{"--model", "claude-opus-5"}, {"--effort", "high"}, {"--mcp-config", "/tmp/task/mcp.json"}, {"--settings", "/tmp/settings.json"}, {"--add-dir", "/extra"}, {"--verbose"}} {
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
	if len(none) != 0 {
		t.Fatalf("no options should produce no args, got %v", none)
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
