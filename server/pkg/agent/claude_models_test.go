package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// The two fixtures under testdata are verbatim stdout from a real
// `claude --print --input-format stream-json --output-format stream-json`
// answering our list_models control request. They are the whole point of
// MUL-6961 and are captured, not written by hand:
//
//   - 2.1.258 offers Fable 5.1 as a normal selectable row.
//   - 2.1.246 — the build whose 400 opened the issue — does not offer it at
//     all. It reports a disabled row instead, carrying the upstream remedy
//     ("Update to 2.1.255+ to use Fable 5.1"). That row is the reason Multica
//     does not need a per-model minimum-version table: the CLI already knows.
func loadClaudeListModelsFixture(t *testing.T, version string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude-code-"+version+"-list-models.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

// TestClaudeListModelsArgs_Pinned locks the discovery argv. Every element is
// load-bearing and none is obvious from the call site, which is exactly when a
// pin earns its keep — dropping --verbose or the input format silently turns
// the probe into a session that never answers.
func TestClaudeListModelsArgs_Pinned(t *testing.T) {
	t.Parallel()
	want := []string{
		"--print",
		"--verbose",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--strict-mcp-config",
	}
	if !reflect.DeepEqual(claudeListModelsArgs, want) {
		t.Fatalf("claudeListModelsArgs drifted: got %v, want %v", claudeListModelsArgs, want)
	}
	// A model flag here would make discovery report the catalog for one model
	// instead of the account's, and a prompt would bill a real API call.
	for _, arg := range claudeListModelsArgs {
		if arg == "--model" || arg == "-p" {
			t.Errorf("discovery argv must not pin a model or pass a prompt: %v", claudeListModelsArgs)
		}
	}
}

func TestParseClaudeModelCatalog_RealCLIOutput(t *testing.T) {
	t.Parallel()
	infos, err := parseClaudeModelCatalog(loadClaudeListModelsFixture(t, "2.1.258"))
	if err != nil {
		t.Fatalf("parseClaudeModelCatalog: %v", err)
	}
	if len(infos) != 5 {
		t.Fatalf("got %d rows, want 5", len(infos))
	}
	if infos[0].Value != claudeDefaultModelValue {
		t.Errorf("first row = %q, want the %q sentinel", infos[0].Value, claudeDefaultModelValue)
	}
	if infos[2].ResolvedModel != "claude-fable-5-1" {
		t.Errorf("fable row resolved to %q, want claude-fable-5-1", infos[2].ResolvedModel)
	}
}

// TestParseClaudeModelCatalog_SkipsUnrelatedLines proves the parser reads a
// stream rather than a single document: stdout is shared with whatever else the
// session prints, so noise before the answer must not be mistaken for failure.
func TestParseClaudeModelCatalog_SkipsUnrelatedLines(t *testing.T) {
	t.Parallel()
	answer := string(loadClaudeListModelsFixture(t, "2.1.258"))
	raw := "not json at all\n" +
		`{"type":"system","subtype":"init","session_id":"x"}` + "\n" +
		`{"type":"control_response","response":{"subtype":"success","request_id":"someone-else","response":{"models":[]}}}` + "\n" +
		answer
	infos, err := parseClaudeModelCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("parseClaudeModelCatalog: %v", err)
	}
	if len(infos) != 5 {
		t.Fatalf("got %d rows, want 5 — the reply was not matched by request id", len(infos))
	}
}

// TestParseClaudeModelCatalog_ErrorSubtype covers the old-CLI path. This is the
// reply every build without list_models sends, and it is why discovery needs no
// version gate: the failure is explicit, immediate, and cheap.
func TestParseClaudeModelCatalog_ErrorSubtype(t *testing.T) {
	t.Parallel()
	raw := `{"type":"control_response","response":{"subtype":"error","request_id":"multica-list-models","error":"Unsupported control request subtype: list_models"}}`
	_, err := parseClaudeModelCatalog([]byte(raw))
	if err == nil {
		t.Fatal("expected an error for an error-subtype response")
	}
	if !strings.Contains(err.Error(), "Unsupported control request subtype") {
		t.Errorf("error should quote the runtime verbatim, got %q", err)
	}
}

func TestParseClaudeModelCatalog_NoResponse(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"empty":    "",
		"junk":     "hello\nworld\n",
		"wrong id": `{"type":"control_response","response":{"subtype":"success","request_id":"other","response":{"models":[]}}}`,
	} {
		if _, err := parseClaudeModelCatalog([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

// TestClaudeModelsFromInfos_CurrentCLI pins the whole projection against real
// 2.1.258 output: the `default` sentinel folds into a badge rather than
// becoming a pickable row, the two rows resolving to Opus collapse to one, and
// the context-window tag survives because `claude-opus-5[1m]` is what the CLI
// would actually run for that row.
func TestClaudeModelsFromInfos_CurrentCLI(t *testing.T) {
	t.Parallel()
	infos, err := parseClaudeModelCatalog(loadClaudeListModelsFixture(t, "2.1.258"))
	if err != nil {
		t.Fatalf("parseClaudeModelCatalog: %v", err)
	}
	models := claudeModelsFromInfos(infos)

	wantIDs := []string{
		"claude-opus-5[1m]",
		"claude-fable-5-1",
		"claude-sonnet-5",
		"claude-haiku-4-5-20251001",
	}
	gotIDs := make([]string, 0, len(models))
	for _, m := range models {
		gotIDs = append(gotIDs, m.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("catalog ids = %v, want %v", gotIDs, wantIDs)
	}

	if models[0].Label != "Opus (1M context)" {
		t.Errorf("opus label = %q, want the row's own display name", models[0].Label)
	}
	if !models[0].Default {
		t.Error("the model the `default` row resolves to should carry the Default badge")
	}
	for _, m := range models[1:] {
		if m.Default {
			t.Errorf("%s must not be flagged default; only one entry may be", m.ID)
		}
	}
	for _, m := range models {
		if m.Provider != "anthropic" {
			t.Errorf("%s provider = %q, want anthropic", m.ID, m.Provider)
		}
		if m.Disabled {
			t.Errorf("%s is unexpectedly disabled on a current CLI", m.ID)
		}
	}

	// Effort levels come from the row itself, replacing the `claude --help`
	// scrape plus the hand-kept claudeModelEffortAllow table.
	opus := models[0]
	if opus.Thinking == nil {
		t.Fatal("opus should advertise a thinking catalog")
	}
	gotLevels := make([]string, 0, len(opus.Thinking.SupportedLevels))
	for _, l := range opus.Thinking.SupportedLevels {
		gotLevels = append(gotLevels, l.Value)
	}
	if want := []string{"low", "medium", "high", "xhigh", "max"}; !reflect.DeepEqual(gotLevels, want) {
		t.Errorf("opus levels = %v, want %v", gotLevels, want)
	}
	// The rows carry no default-effort field, and inventing one is what the
	// static path did. Empty means "the runtime decides".
	if opus.Thinking.DefaultLevel != "" {
		t.Errorf("DefaultLevel = %q, want empty", opus.Thinking.DefaultLevel)
	}
	// Haiku advertises no effort support at all, so it must get no picker.
	if haiku := models[3]; haiku.Thinking != nil {
		t.Errorf("haiku advertises no effort support; Thinking should be nil, got %+v", haiku.Thinking)
	}
}

// TestClaudeModelsFromInfos_OldCLIDisabledRow is the regression this whole
// change exists for. On 2.1.246 the catalog must NOT offer Fable 5.1 as
// selectable — that combination is the guaranteed 400 from the issue — and must
// still tell the user it exists and how to reach it.
func TestClaudeModelsFromInfos_OldCLIDisabledRow(t *testing.T) {
	t.Parallel()
	infos, err := parseClaudeModelCatalog(loadClaudeListModelsFixture(t, "2.1.246"))
	if err != nil {
		t.Fatalf("parseClaudeModelCatalog: %v", err)
	}
	models := claudeModelsFromInfos(infos)

	for _, m := range models {
		if m.ID == "claude-fable-5-1" && !m.Disabled {
			t.Fatal("2.1.246 must never offer claude-fable-5-1 as selectable: the run 400s")
		}
	}

	var disabled *Model
	for i := range models {
		if models[i].Disabled {
			disabled = &models[i]
		}
	}
	if disabled == nil {
		t.Fatal("expected a disabled row carrying the update hint")
	}
	if disabled.Label != "Fable 5.1 (disabled)" {
		t.Errorf("disabled label = %q", disabled.Label)
	}
	if disabled.DisabledReason != "Update to 2.1.255+ to use Fable 5.1" {
		t.Errorf("disabled reason = %q, want the runtime's own upgrade hint", disabled.DisabledReason)
	}
	// Fable 5 is the model this CLI *can* run, and it stays selectable.
	found := false
	for _, m := range models {
		if m.ID == "claude-fable-5" && !m.Disabled {
			found = true
		}
	}
	if !found {
		t.Error("claude-fable-5 should remain selectable on 2.1.246")
	}
}

func TestClaudeModelsFromInfos_DefaultWithNoSiblingRow(t *testing.T) {
	t.Parallel()
	// An org-restricted install can resolve `default` to a model no other row
	// names. Folding the sentinel away must not drop that model entirely.
	models := claudeModelsFromInfos([]claudeModelInfo{
		{Value: "default", ResolvedModel: "claude-sonnet-5", DisplayName: "Default (recommended)"},
		{Value: "haiku", ResolvedModel: "claude-haiku-4-5-20251001", DisplayName: "Haiku"},
	})
	if len(models) != 1 || models[0].ID != "claude-haiku-4-5-20251001" {
		t.Fatalf("unexpected catalog %+v", models)
	}
	if models[0].Default {
		t.Error("a model the default row does not resolve to must not be badged")
	}
}

// TestDiscoverClaudeModels_ArgvAndStdin runs the real discovery path against a
// stand-in binary, checking both halves of the contract: the argv the process
// receives, and that the control request actually arrives on its stdin. A probe
// that spawns correctly but never writes the request would hang in production
// and pass a parser-only test.
func TestDiscoverClaudeModels_ArgvAndStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	stdinFile := filepath.Join(dir, "stdin.txt")
	fixture := filepath.Join(dir, "reply.jsonl")
	if err := os.WriteFile(fixture, loadClaudeListModelsFixture(t, "2.1.258"), 0o600); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}
	fake := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + argvFile + "'\n" +
		"cat > '" + stdinFile + "'\n" +
		"cat '" + fixture + "'\n"
	writeTestExecutable(t, fake, []byte(script))

	models, err := discoverClaudeModels(context.Background(), Command{Path: fake})
	if err != nil {
		t.Fatalf("discoverClaudeModels: %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("got %d models, want 4", len(models))
	}

	gotArgv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	if got := splitNonEmptyLines(string(gotArgv)); !reflect.DeepEqual(got, claudeListModelsArgs) {
		t.Errorf("fake claude received argv %v, want %v", got, claudeListModelsArgs)
	}

	gotStdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if !strings.Contains(string(gotStdin), `"subtype":"list_models"`) {
		t.Errorf("control request never reached stdin, got %q", gotStdin)
	}
	if !strings.Contains(string(gotStdin), claudeListModelsRequestID) {
		t.Errorf("request id missing from stdin, got %q", gotStdin)
	}
	if !strings.HasSuffix(string(gotStdin), "\n") {
		t.Error("request must be newline-terminated or the CLI never parses the line")
	}
}

func TestDiscoverClaudeModels_ErrorPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary requires a POSIX shell")
	}
	t.Parallel()

	for name, body := range map[string]string{
		// The reply an old CLI without list_models sends.
		"unsupported subtype": `echo '{"type":"control_response","response":{"subtype":"error","request_id":"multica-list-models","error":"Unsupported control request subtype: list_models"}}'` + "\n",
		"no reply":            "exit 0\n",
		"garbage":             "echo 'not json'\n",
		// A well-formed reply with nothing usable must not pass as a catalog.
		"empty catalog": `echo '{"type":"control_response","response":{"subtype":"success","request_id":"multica-list-models","response":{"models":[]}}}'` + "\n",
		// Only the follow-the-CLI sentinel, which is not a pickable model.
		"default row only": `echo '{"type":"control_response","response":{"subtype":"success","request_id":"multica-list-models","response":{"models":[{"value":"default","resolvedModel":"claude-opus-5"}]}}}'` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := filepath.Join(t.TempDir(), "claude")
			writeTestExecutable(t, fake, []byte("#!/bin/sh\ncat > /dev/null\n"+body))
			if _, err := discoverClaudeModels(context.Background(), Command{Path: fake}); err == nil {
				t.Fatal("expected an error so the caller falls back to the static catalog")
			}
		})
	}
}

// TestDiscoverClaudeCatalog_FallsBackToStatic covers the degrade path. The
// Fallback flag is the load-bearing part: it is what stops the server caching a
// discovery failure as this runtime's catalog for a day (MUL-5549).
func TestDiscoverClaudeCatalog_FallsBackToStatic(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "definitely-not-installed")

	catalog := discoverClaudeCatalog(context.Background(), Command{Path: missing})
	if !catalog.Fallback {
		t.Error("a static answer after discovery failed must be flagged Fallback")
	}
	if len(catalog.Models) != len(claudeStaticModels()) {
		t.Errorf("got %d models, want the static catalog's %d",
			len(catalog.Models), len(claudeStaticModels()))
	}
}

func TestDiscoverClaudeCatalog_LiveResultIsAuthoritative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary requires a POSIX shell")
	}
	t.Parallel()

	dir := t.TempDir()
	fixture := filepath.Join(dir, "reply.jsonl")
	if err := os.WriteFile(fixture, loadClaudeListModelsFixture(t, "2.1.246"), 0o600); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}
	fake := filepath.Join(dir, "claude")
	writeTestExecutable(t, fake, []byte("#!/bin/sh\ncat > /dev/null\ncat '"+fixture+"'\n"))

	catalog := discoverClaudeCatalog(context.Background(), Command{Path: fake})
	if catalog.Fallback {
		t.Error("a live catalog must not be flagged Fallback — the server should cache it")
	}
	for _, m := range catalog.Models {
		if m.ID == "claude-fable-5-1" {
			t.Fatal("2.1.246's live catalog must not contain claude-fable-5-1")
		}
	}
}

// TestValidateThinkingLevel_TaggedCatalogID guards the capability lookup against
// the context-window tag discovery now puts in catalog ids. Without normalising
// the catalog side, `claude-opus-5[1m]` never matches the stripped target and
// every effort level fails closed — the daemon would silently drop --effort.
func TestValidateThinkingLevel_TaggedCatalogID(t *testing.T) {
	t.Parallel()
	catalog := Catalog{Models: []Model{{
		ID:       "claude-opus-5[1m]",
		Label:    "Opus (1M context)",
		Provider: "anthropic",
		Thinking: &ModelThinking{SupportedLevels: []ThinkingLevel{
			{Value: "low"}, {Value: "medium"}, {Value: "high"}, {Value: "xhigh"}, {Value: "max"},
		}},
	}}}
	load := func() (Catalog, error) { return catalog, nil }

	for _, model := range []string{"claude-opus-5[1m]", "claude-opus-5"} {
		ok, err := ValidateThinkingLevelWith(load, "claude", model, "xhigh")
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if !ok {
			t.Errorf("%s: xhigh should validate against the tagged catalog entry", model)
		}
	}

	ok, err := ValidateThinkingLevelWith(load, "claude", "claude-opus-5", "nonsense")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("a level the catalog does not advertise must still fail closed")
	}
}
