package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// workspaceCoAuthoredByEnabled gates the prepare-commit-msg hook installed in
// agent worktrees. RFC MUL-2414 adds the `github_enabled` master switch:
// when it is explicitly false the hook must NOT be installed even if
// `co_authored_by_enabled` is true. The function also defaults to true
// whenever settings are absent or malformed so existing workspaces keep
// their historical behavior.
func TestWorkspaceCoAuthoredByEnabled(t *testing.T) {
	cases := []struct {
		name     string
		register bool
		settings string
		want     bool
	}{
		{"unknown workspace defaults on", false, "", true},
		{"registered workspace, nil settings defaults on", true, "", true},
		{"empty object defaults on", true, "{}", true},
		{"co_authored_by absent defaults on", true, `{"github_enabled":true}`, true},
		{"co_authored_by true", true, `{"co_authored_by_enabled":true}`, true},
		{"co_authored_by false", true, `{"co_authored_by_enabled":false}`, false},
		{
			"master off forces hook off even when co_authored_by true",
			true,
			`{"github_enabled":false,"co_authored_by_enabled":true}`,
			false,
		},
		{
			"master on lets co_authored_by decide",
			true,
			`{"github_enabled":true,"co_authored_by_enabled":false}`,
			false,
		},
		{"malformed settings defaults on", true, `not json`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{workspaces: make(map[string]*workspaceState)}
			if tc.register {
				var raw json.RawMessage
				if tc.settings != "" {
					raw = json.RawMessage(tc.settings)
				}
				d.workspaces["ws"] = newWorkspaceState("ws", nil, "", nil, raw)
			}
			if got := d.workspaceCoAuthoredByEnabled("ws"); got != tc.want {
				t.Fatalf("workspaceCoAuthoredByEnabled(%q) = %v, want %v",
					tc.settings, got, tc.want)
			}
		})
	}
}

// syncWorkspacesFromAPI must not refresh repos or settings for an already-
// tracked workspace. They are only consumed by repo checkout, whose
// ensureRepoReady path refreshes them immediately before use. Keeping this
// periodic sync to workspace/runtime duties prevents idle daemons from
// repeatedly hitting the workspace repos endpoint.
func TestSyncWorkspacesSkipsReposRefreshOnExistingWorkspace(t *testing.T) {
	t.Parallel()

	const workspaceID = "ws-1"

	var repoCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/daemon/workspaces":
			json.NewEncoder(w).Encode([]WorkspaceInfo{{ID: workspaceID, Name: "ws"}})
		case "/api/daemon/workspaces/" + workspaceID + "/repos":
			repoCalls.Add(1)
			json.NewEncoder(w).Encode(WorkspaceReposResponse{
				WorkspaceID:  workspaceID,
				Repos:        []RepoData{},
				ReposVersion: "v1",
				Settings:     json.RawMessage(`{"github_enabled":false,"co_authored_by_enabled":true}`),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:       NewClient(srv.URL),
		logger:       slog.Default(),
		workspaces:   make(map[string]*workspaceState),
		runtimeIndex: make(map[string]Runtime),
		runtimeSet:   newRuntimeSetWatcher(),
	}
	// Pretend the workspace was already registered with co-author ON. A live
	// runtime ID keeps workspaceNeedsRuntimeRecovery from short-circuiting the
	// sync into a re-register.
	d.workspaces[workspaceID] = newWorkspaceState(
		workspaceID,
		[]string{"rt-1"},
		"v1",
		nil,
		json.RawMessage(`{"github_enabled":true,"co_authored_by_enabled":true}`),
	)

	if !d.workspaceCoAuthoredByEnabled(workspaceID) {
		t.Fatalf("precondition: expected co-author hook to start enabled")
	}

	if err := d.syncWorkspacesFromAPI(context.Background(), false); err != nil {
		t.Fatalf("syncWorkspacesFromAPI: %v", err)
	}

	if got := repoCalls.Load(); got != 0 {
		t.Fatalf("workspace sync called repos endpoint %d times, want 0", got)
	}

	if !d.workspaceCoAuthoredByEnabled(workspaceID) {
		t.Fatal("workspace sync unexpectedly replaced cached settings")
	}
}

// coAuthoredByStateCache is a repoCacheBackend that records what the daemon
// publishes for installed prepare-commit-msg hooks to read.
type coAuthoredByStateCache struct {
	mu     sync.Mutex
	writes []bool
}

func (c *coAuthoredByStateCache) Lookup(string, string) string   { return "" }
func (c *coAuthoredByStateCache) BarePath(string, string) string { return "" }
func (c *coAuthoredByStateCache) Sync(string, []repocache.RepoInfo) error {
	return nil
}
func (c *coAuthoredByStateCache) WithRepoLock(_ string, fn func() error) error { return fn() }
func (c *coAuthoredByStateCache) CreateWorktree(repocache.WorktreeParams) (*repocache.WorktreeResult, error) {
	return nil, nil
}

func (c *coAuthoredByStateCache) WriteCoAuthoredByState(_ string, enabled bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, enabled)
	return nil
}

func (c *coAuthoredByStateCache) lastWrite(t *testing.T) bool {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.writes) == 0 {
		t.Fatal("no Co-authored-by state was published")
	}
	return c.writes[len(c.writes)-1]
}

// newCoAuthoredByStateDaemon returns a daemon tracking one workspace whose
// settings the fake server serves from settings, plus the cache recording
// published state.
func newCoAuthoredByStateDaemon(t *testing.T, workspaceID string, settings *string) (*Daemon, *coAuthoredByStateCache) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces/"+workspaceID+"/repos" {
			http.NotFound(w, r)
			return
		}
		resp := WorkspaceReposResponse{WorkspaceID: workspaceID, ReposVersion: "v1"}
		if settings != nil {
			resp.Settings = json.RawMessage(*settings)
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	cache := &coAuthoredByStateCache{}
	d := &Daemon{
		cfg:        Config{CLIVersion: "v1.0.0"},
		client:     NewClient(srv.URL),
		repoCache:  cache,
		workspaces: map[string]*workspaceState{workspaceID: newWorkspaceState(workspaceID, nil, "", nil, nil)},
		logger:     slog.Default(),
	}
	return d, cache
}

// A settings refresh must publish the current verdict to the repo cache, not
// just to the daemon's in-memory copy: prepare-commit-msg hooks installed by
// earlier checkouts read that file on every commit, and it is the only way a
// toggle-off reaches a checkout that already exists (MUL-6921).
func TestRefreshWorkspaceReposPublishesCoAuthoredByState(t *testing.T) {
	t.Parallel()

	const workspaceID = "ws-1"
	settings := `{"co_authored_by_enabled":false}`
	d, cache := newCoAuthoredByStateDaemon(t, workspaceID, &settings)

	if _, err := d.refreshWorkspaceRepos(context.Background(), workspaceID); err != nil {
		t.Fatalf("refreshWorkspaceRepos failed: %v", err)
	}
	if cache.lastWrite(t) {
		t.Error("published state = enabled, want disabled after the toggle was turned off")
	}

	settings = `{"co_authored_by_enabled":true}`
	if _, err := d.refreshWorkspaceRepos(context.Background(), workspaceID); err != nil {
		t.Fatalf("refreshWorkspaceRepos failed: %v", err)
	}
	if !cache.lastWrite(t) {
		t.Error("published state = disabled, want enabled after the toggle was turned back on")
	}
}

// The server's workspaces-changed hint is the only signal a running daemon
// gets for a settings edit — the periodic sync makes no repos/settings request
// for a workspace it already tracks. refreshTrackedWorkspaceSettings is what
// turns that hint into a new verdict without waiting for the next checkout.
func TestRefreshTrackedWorkspaceSettingsAppliesToggle(t *testing.T) {
	t.Parallel()

	const workspaceID = "ws-1"
	settings := `{"co_authored_by_enabled":false}`
	d, cache := newCoAuthoredByStateDaemon(t, workspaceID, &settings)

	if !d.workspaceCoAuthoredByEnabled(workspaceID) {
		t.Fatal("precondition: workspace should start with the default (enabled) verdict")
	}

	d.refreshTrackedWorkspaceSettings(context.Background())

	if d.workspaceCoAuthoredByEnabled(workspaceID) {
		t.Error("daemon still reports the trailer as enabled after the workspace disabled it")
	}
	if cache.lastWrite(t) {
		t.Error("published state = enabled, want disabled")
	}
}

// A daemon that starts (or restarts) after the toggle was flipped learns the
// new value from its register response, and nothing else would ever carry it
// to the hooks already on disk. The workspace sync republishes it locally —
// without touching the repos endpoint, which the sibling test above pins.
func TestSyncWorkspacesPublishesCoAuthoredByState(t *testing.T) {
	t.Parallel()

	const workspaceID = "ws-1"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode([]WorkspaceInfo{{ID: workspaceID, Name: "ws"}})
	}))
	t.Cleanup(srv.Close)

	cache := &coAuthoredByStateCache{}
	d := &Daemon{
		client:       NewClient(srv.URL),
		logger:       slog.Default(),
		repoCache:    cache,
		workspaces:   make(map[string]*workspaceState),
		runtimeIndex: make(map[string]Runtime),
		runtimeSet:   newRuntimeSetWatcher(),
	}
	d.workspaces[workspaceID] = newWorkspaceState(
		workspaceID,
		[]string{"rt-1"},
		"v1",
		nil,
		json.RawMessage(`{"co_authored_by_enabled":false}`),
	)

	if err := d.syncWorkspacesFromAPI(context.Background(), false); err != nil {
		t.Fatalf("syncWorkspacesFromAPI: %v", err)
	}

	if cache.lastWrite(t) {
		t.Error("published state = enabled, want disabled for a workspace whose settings say so")
	}
}
