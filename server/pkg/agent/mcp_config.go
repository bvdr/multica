package agent

import (
	"bytes"
	"encoding/json"
)

// hasManagedMcpConfig preserves the API's three-state MCP semantics. Only SQL
// NULL / JSON null mean "inherit the runtime configuration"; any object,
// including an explicitly empty one, is a managed set and enables strict mode.
func hasManagedMcpConfig(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// HasManagedMcpConfig is the exported form for the daemon's tmux runner
// (ContextPRO fork), which writes the config to a file itself instead of going
// through Execute.
func HasManagedMcpConfig(raw json.RawMessage) bool { return hasManagedMcpConfig(raw) }
