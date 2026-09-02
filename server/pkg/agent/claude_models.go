package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── Claude model discovery ───────────────────────────────────────────
//
// Claude Code has no `--list-models` flag and no models subcommand, which is
// why this package carried a hand-maintained claudeStaticModels() for so long.
// It does have a discovery hook, just not on the command line: the stream-json
// control protocol answers a `list_models` control request with the same
// ModelInfo rows the interactive /model picker renders.
//
// That distinction is the whole point of MUL-6961. A static catalog cannot know
// what the CLI in front of it can actually run, so every new Anthropic model
// opened a window where Multica offered a model that 400s ("Claude Code 2.1.246
// does not support this model; version 2.1.251 or newer is required"). The
// control protocol closes the window at the source: the answer is computed by
// the installed binary, against the logged-in account, so a model the local CLI
// cannot run is either absent or explicitly flagged disabled. Verified against
// 2.1.223 / 2.1.228 / 2.1.246 / 2.1.258 — the three older builds return Fable
// 5.1 as a disabled row carrying "Update to 2.1.255+ to use Fable 5.1", and
// 2.1.258 returns it as a normal selectable entry.
//
// No version gate guards the request, unlike codexSupportsDebugModels. It would
// buy nothing: an unsupported subtype is answered, not ignored — every build
// tested replies `Unsupported control request subtype: ...` in about two
// seconds and exits 0 — so an old CLI costs one cheap round trip and falls back,
// with no risk of hanging until the timeout. Gating instead on a version floor
// would mean hand-maintaining exactly the kind of number this change exists to
// stop hand-maintaining.

// claudeListModelsArgs is the argv for a discovery-only Claude session.
//
// `--print` with stream-json on both ends is the control-protocol channel the
// daemon already speaks for task execution. No user message is ever written, so
// no model is invoked and nothing is billed: the process answers the control
// request, sees stdin close, and exits 0.
//
// `--strict-mcp-config` without any `--mcp-config` resolves to "no MCP servers
// at all". Discovery has no use for the user's servers and every reason not to
// boot them — a stdio server that spawns a container would make enumerating a
// model list arbitrarily slow and arbitrarily side-effecting.
//
// Kept as a package-level var rather than a literal at the call site so tests
// can pin the exact argv a real `claude` invocation receives; the argv shape is
// as much of the contract as the parser is.
var claudeListModelsArgs = []string{
	"--print",
	"--verbose",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--strict-mcp-config",
}

// claudeListModelsRequestID labels our control request so the reply can be
// picked out of the stream. Claude answers a discovery-only session in a single
// stdout line today, but matching on the id keeps that from being load-bearing.
const claudeListModelsRequestID = "multica-list-models"

// claudeListModelsTimeout bounds one discovery round trip. Observed cost is
// 1.6–1.9s warm and ~0.2s against an empty config dir; the ceiling is generous
// because a cold start on a slow disk is the case worth surviving, and the only
// thing waiting on it is a fallback to the static catalog.
const claudeListModelsTimeout = 20 * time.Second

// claudeModelInfo is one row of the control protocol's model catalog.
//
// Value is the picker token (`sonnet`, `opus[1m]`, or the sentinel `default`);
// ResolvedModel is what that token actually runs and is what Multica persists.
// Disabled marks a row the CLI shows greyed out — visible on purpose, so the
// user learns the model exists and why it is out of reach, with Description
// carrying the runtime's own remedy.
type claudeModelInfo struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
	Disabled              bool     `json:"disabled"`
}

// claudeControlResponse is the control-protocol envelope. The doubled
// `response` nesting is Claude's shape, not a transcription slip: the outer one
// is the envelope (subtype/request_id/error), the inner one is the payload.
type claudeControlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
		Response  struct {
			Models []claudeModelInfo `json:"models"`
		} `json:"response"`
	} `json:"response"`
}

// claudeDefaultModelValue is the picker row meaning "whatever this CLI resolves
// to", not a model in its own right. Multica already spells that "leave the
// agent's model empty", so the row is folded into the Default flag rather than
// offered as a pick.
const claudeDefaultModelValue = "default"

// discoverClaudeCatalog answers a model-listing round for claude, preferring
// the live catalog and degrading to the hand-maintained one.
//
// The fallback is flagged, which is a change in kind rather than a detail. The
// static list used to be returned as authoritative because there was nothing to
// fall back from; now that discovery exists, a static answer means discovery
// failed, and Catalog.Fallback is what stops the server pinning that failure in
// a day-long cache (MUL-5549). The cost is that a CLI too old to answer
// list_models never gets a cached catalog — correct, and the same deal every
// other fallback provider already takes.
func discoverClaudeCatalog(ctx context.Context, runtimeCmd Command) Catalog {
	models, err := discoverClaudeModels(ctx, runtimeCmd)
	if err == nil {
		return Catalog{Models: models}
	}
	if runtimeCmd.logger != nil {
		runtimeCmd.logger.Debug("claude model discovery failed, using static catalog", "error", err)
	}
	static := claudeStaticModels()
	annotateClaudeThinking(ctx, static, runtimeCmd)
	return Catalog{Models: static, Fallback: true}
}

// discoverClaudeModels enumerates the local Claude Code catalog over the
// control protocol. A failure at any stage — spawn, timeout, malformed reply,
// error subtype, empty list — is returned as an error so the caller can fall
// back to the static catalog; no partial result is ever reported as authoritative.
func discoverClaudeModels(ctx context.Context, runtimeCmd Command) ([]Model, error) {
	if runtimeCmd.Path == "" {
		runtimeCmd.Path = "claude"
	}

	raw, err := runClaudeListModels(ctx, runtimeCmd)
	if err != nil {
		return nil, err
	}
	infos, err := parseClaudeModelCatalog(raw)
	if err != nil {
		return nil, err
	}
	models := claudeModelsFromInfos(infos)
	if len(models) == 0 {
		return nil, errors.New("claude list_models returned no usable models")
	}
	return models, nil
}

func runClaudeListModels(ctx context.Context, runtimeCmd Command) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeListModelsTimeout)
	defer cancel()

	request, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": claudeListModelsRequestID,
		"request":    map[string]any{"subtype": "list_models"},
	})
	if err != nil {
		return nil, err
	}

	cmd := runtimeCmd.exec(ctx, claudeListModelsArgs...)
	// Feeding stdin from a reader rather than a pipe is what makes this a
	// one-shot probe: os/exec closes the pipe once the request is written, the
	// CLI takes that EOF as end of session and exits on its own. Nothing here
	// has to police a lingering process.
	cmd.Stdin = strings.NewReader(string(request) + "\n")
	hideAgentWindow(cmd)
	return outputOwned(cmd, runtimeCmd.logger)
}

// parseClaudeModelCatalog pulls our reply out of the stream-json stdout.
//
// Non-JSON and unrelated lines are skipped rather than treated as corruption:
// stdout is a stream shared with whatever else the session emits, and a
// discovery run that failed to find its own answer is already reported as an
// error below.
func parseClaudeModelCatalog(raw []byte) ([]claudeModelInfo, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp claudeControlResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		if resp.Type != "control_response" || resp.Response.RequestID != claudeListModelsRequestID {
			continue
		}
		if resp.Response.Subtype != "success" {
			// An old CLI lands here with "Unsupported control request
			// subtype: list_models". Surfacing the runtime's own words keeps
			// the daemon log honest about which of the two failures it hit.
			reason := strings.TrimSpace(resp.Response.Error)
			if reason == "" {
				reason = resp.Response.Subtype
			}
			return nil, fmt.Errorf("claude list_models failed: %s", reason)
		}
		return resp.Response.Response.Models, nil
	}
	return nil, errors.New("claude list_models produced no control response")
}

// claudeModelsFromInfos projects the control-protocol rows onto the catalog.
//
// Rows are keyed by ResolvedModel, not by the picker token: two tokens
// routinely resolve to one model (`default` and `opus[1m]` both mean Opus 5
// with a 1M window), and it is the resolved name that gets persisted on the
// agent, passed to `--model`, and matched by the pricing table. The context
// window tag rides along on purpose — `claude-opus-5[1m]` is what the CLI would
// actually run for that row, so dropping the tag would quietly downgrade a user
// who picked "Opus (1M context)" to the default window.
func claudeModelsFromInfos(infos []claudeModelInfo) []Model {
	models := make([]Model, 0, len(infos))
	index := make(map[string]int, len(infos))
	defaultResolved := ""

	for _, info := range infos {
		id := strings.TrimSpace(info.ResolvedModel)
		if id == "" {
			id = strings.TrimSpace(info.Value)
		}
		if id == "" {
			continue
		}
		if strings.TrimSpace(info.Value) == claudeDefaultModelValue {
			// Not an entry of its own — remember what it points at so the row
			// that does carry that model gets the badge.
			defaultResolved = id
			continue
		}
		if _, seen := index[id]; seen {
			continue
		}
		label := strings.TrimSpace(info.DisplayName)
		if label == "" {
			label = id
		}
		entry := Model{
			ID:       id,
			Label:    label,
			Provider: "anthropic",
			Thinking: claudeThinkingFromInfo(info),
		}
		if info.Disabled {
			entry.Disabled = true
			entry.DisabledReason = strings.TrimSpace(info.Description)
		}
		index[id] = len(models)
		models = append(models, entry)
	}

	if defaultResolved != "" {
		if i, ok := index[defaultResolved]; ok {
			models[i].Default = true
		}
	}
	return models
}

// claudeThinkingFromInfo builds the per-model effort catalog from the row's own
// advertisement. This is the discovery path's clearest win over the static one:
// loadClaudeThinkingByModel has to scrape `claude --help` for a global superset
// and then narrow it through claudeModelEffortAllow, a hand-kept table of which
// models really take xhigh. Here each model states its own levels.
//
// DefaultLevel is deliberately left empty. The rows carry no default-effort
// field, and empty already means "the runtime picks, we don't know" — a more
// honest answer than the static path's assumed "medium".
func claudeThinkingFromInfo(info claudeModelInfo) *ModelThinking {
	if !info.SupportsEffort || len(info.SupportedEffortLevels) == 0 {
		return nil
	}
	levels := make([]ThinkingLevel, 0, len(info.SupportedEffortLevels))
	for _, value := range info.SupportedEffortLevels {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		label, ok := claudeEffortLabel[value]
		if !ok {
			// A level this daemon has not been taught yet. Show it raw rather
			// than hide it — the CLI is the authority on what it accepts.
			label = strings.ToUpper(value[:1]) + value[1:]
		}
		levels = append(levels, ThinkingLevel{Value: value, Label: label})
	}
	if len(levels) == 0 {
		return nil
	}
	return &ModelThinking{SupportedLevels: levels}
}
