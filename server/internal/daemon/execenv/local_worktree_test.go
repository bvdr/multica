package execenv

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func worktreeTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestRepo creates a git repo with one commit and returns its path. The
// repo carries its own identity config so tests don't depend on the machine's
// global git setup.
func newTestRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	// macOS hands out /var/folders temp dirs that are symlinks to /private/var.
	// resolveGitRoot canonicalises, so the test must compare against the
	// canonical form too.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.name", "Test User")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	writeFile(t, filepath.Join(dir, "tracked.txt"), "original\n")
	writeFile(t, filepath.Join(dir, "keep.txt"), "keep\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial")
	return dir
}

// requireGit skips the whole test when git is missing from the environment.
// This is the ONLY place a skip is allowed: once a test is past it, every git
// command is part of the assertion surface, so a failure there is a real
// regression and must fail loudly. An earlier version skipped on any git error,
// which would have turned "the delivered branch is gone" into a green run.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not available: %v", err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func gitTry(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func prepareForTest(t *testing.T, localPath string) *LocalWorktree {
	t.Helper()
	wt, err := PrepareLocalWorktree(LocalWorktreeParams{
		LocalPath: localPath,
		EnvRoot:   t.TempDir(),
		AgentName: "J",
		TaskID:    "11112222-3333-4444-5555-666677778888",
	}, worktreeTestLogger())
	if err != nil {
		t.Fatalf("PrepareLocalWorktree: %v", err)
	}
	return wt
}

// The agent must see the user's uncommitted work, not a clean HEAD checkout.
// This is the property that makes worktree mode usable rather than confusing:
// otherwise the agent silently reviews code the user hasn't got open.
func TestPrepareLocalWorktreeReplaysUncommittedWork(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "edited by user\n")
	writeFile(t, filepath.Join(repo, "brand-new.txt"), "untracked\n")
	writeFile(t, filepath.Join(repo, "nested/deep.txt"), "nested untracked\n")

	wt := prepareForTest(t, repo)

	if got := readFile(t, filepath.Join(wt.Path, "tracked.txt")); got != "edited by user\n" {
		t.Errorf("tracked edit not replayed into worktree: got %q", got)
	}
	if got := readFile(t, filepath.Join(wt.Path, "brand-new.txt")); got != "untracked\n" {
		t.Errorf("untracked file not copied: got %q", got)
	}
	if got := readFile(t, filepath.Join(wt.Path, "nested/deep.txt")); got != "nested untracked\n" {
		t.Errorf("nested untracked file not copied: got %q", got)
	}
	if !wt.DirtyBaseCaptured {
		t.Error("DirtyBaseCaptured = false, want true")
	}
	if wt.UntrackedCopied != 2 {
		t.Errorf("UntrackedCopied = %d, want 2", wt.UntrackedCopied)
	}
}

// Capturing the dirty state must not disturb the user's own working tree —
// they may have a build running against it. `git stash create` writes a commit
// object but must leave the index, the files, and the stash list alone.
func TestPrepareLocalWorktreeLeavesUserTreeUntouched(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "edited by user\n")
	writeFile(t, filepath.Join(repo, "brand-new.txt"), "untracked\n")
	statusBefore := gitRun(t, repo, "status", "--porcelain")

	prepareForTest(t, repo)

	if got := readFile(t, filepath.Join(repo, "tracked.txt")); got != "edited by user\n" {
		t.Errorf("user's file changed: got %q", got)
	}
	if got := gitRun(t, repo, "status", "--porcelain"); got != statusBefore {
		t.Errorf("user's git status changed:\nbefore %q\nafter  %q", statusBefore, got)
	}
	if got := gitRun(t, repo, "stash", "list"); got != "" {
		t.Errorf("stash list should stay empty, got %q", got)
	}
}

// The deliverable is a branch. Whatever the agent leaves uncommitted must be
// committed onto it before the worktree is removed, or `git worktree remove
// --force` would delete the work with no way back.
func TestFinalizeCommitsLeftoversAndKeepsBranch(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)

	writeFile(t, filepath.Join(wt.Path, "agent-output.txt"), "work product\n")

	outcome := finalizeOK(t, wt)

	if !outcome.AutoCommitted {
		t.Error("AutoCommitted = false, want true")
	}
	if outcome.Branch != wt.Branch {
		t.Errorf("Branch = %q, want %q", outcome.Branch, wt.Branch)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree directory still present after Finalize: %v", err)
	}
	if list := gitRun(t, repo, "worktree", "list"); strings.Contains(list, wt.Path) {
		t.Errorf("worktree still registered in user's repo:\n%s", list)
	}
	// The branch must survive in the user's repo, carrying the agent's file.
	if got := gitRun(t, repo, "show", wt.Branch+":agent-output.txt"); got != "work product" {
		t.Errorf("branch does not carry agent output, got %q", got)
	}
	// The user's own checkout must be untouched by any of it.
	if _, err := os.Stat(filepath.Join(repo, "agent-output.txt")); !os.IsNotExist(err) {
		t.Error("agent output leaked into the user's working tree")
	}
}

// A read-only task changes nothing. Leaving an empty branch behind for every
// such run would turn `git branch` into noise, so the branch is dropped and the
// result reports no branch at all.
func TestFinalizeDropsBranchWhenNothingChanged(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)

	outcome := finalizeOK(t, wt)

	if outcome.Branch != "" {
		t.Errorf("Branch = %q, want empty for a no-op task", outcome.Branch)
	}
	if outcome.AutoCommitted {
		t.Error("AutoCommitted = true, want false")
	}
	if out, err := gitTry(t, repo, "rev-parse", "--verify", wt.Branch); err == nil {
		t.Errorf("empty branch should have been deleted, still resolves to %s", out)
	}
}

// The user's own uncommitted work must not be mistaken for the agent's output.
// It is committed as a baseline at prepare time, so a task that changes nothing
// still counts as a no-op and leaves no branch — the user's WIP is already safe
// in their own working tree, and a branch duplicating it is pure noise.
func TestFinalizeDropsBranchWhenOnlyBaseWasDirty(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "edited by user\n")
	writeFile(t, filepath.Join(repo, "scratch.txt"), "untracked scratch\n")
	wt := prepareForTest(t, repo)

	// The baseline must be a commit of its own, not left as pending changes.
	if wt.BaseCommit == "" {
		t.Fatal("no baseline commit recorded for a dirty base")
	}
	if dirty, err := worktreeIsDirty(wt.Path); err != nil || dirty {
		t.Errorf("worktree still dirty after baseline commit (dirty=%v, err=%v)", dirty, err)
	}

	outcome := finalizeOK(t, wt)

	if outcome.Branch != "" {
		t.Errorf("Branch = %q, want empty: the agent changed nothing", outcome.Branch)
	}
	if outcome.AutoCommitted {
		t.Error("AutoCommitted = true, but there was nothing of the agent's to commit")
	}
}

// With a dirty base, the delivered branch separates the two authorships: the
// user's WIP is the baseline commit, the agent's work sits on top. That is what
// makes `git diff <baseline>..<branch>` a readable review of the agent alone.
func TestFinalizeSeparatesUserBaselineFromAgentWork(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "edited by user\n")
	wt := prepareForTest(t, repo)

	writeFile(t, filepath.Join(wt.Path, "agent-output.txt"), "work product\n")
	outcome := finalizeOK(t, wt)

	if outcome.Branch == "" {
		t.Fatal("no branch delivered for a task that changed a file")
	}
	// The agent's commit alone.
	agentFiles := gitRun(t, repo, "diff", "--name-only", wt.BaseCommit, outcome.Branch)
	if agentFiles != "agent-output.txt" {
		t.Errorf("agent diff = %q, want just agent-output.txt", agentFiles)
	}
	// And the user's uncommitted edit still reached the branch.
	if got := gitRun(t, repo, "show", outcome.Branch+":tracked.txt"); got != "edited by user" {
		t.Errorf("branch lost the user's uncommitted edit, got %q", got)
	}
}

// A resource may point at a subdirectory of a repo. The worktree covers the
// whole repo, but the agent has to land at the same depth the user chose.
func TestPrepareLocalWorktreeSubdirectory(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "services/api/main.go"), "package main\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "add service")

	sub := filepath.Join(repo, "services/api")
	wt := prepareForTest(t, sub)

	if wt.GitRoot != repo {
		t.Errorf("GitRoot = %q, want repo root %q", wt.GitRoot, repo)
	}
	want := filepath.Join(wt.Path, "services", "api")
	if wt.WorkDir != want {
		t.Errorf("WorkDir = %q, want %q", wt.WorkDir, want)
	}
	if got := readFile(t, filepath.Join(wt.WorkDir, "main.go")); got != "package main\n" {
		t.Errorf("subdirectory content missing from worktree: %q", got)
	}
}

// Worktree mode is opt-in per resource, so a non-git directory means the user
// configured something that cannot work. Fail with an actionable message rather
// than silently running in-place, which would leave them wondering why their
// tasks still queue one at a time.
func TestPrepareLocalWorktreeRejectsNonGitDirectory(t *testing.T) {
	_, err := PrepareLocalWorktree(LocalWorktreeParams{
		LocalPath: t.TempDir(),
		EnvRoot:   t.TempDir(),
		TaskID:    "task-1",
	}, worktreeTestLogger())
	if err == nil {
		t.Fatal("expected an error for a non-git directory")
	}
	if !strings.Contains(err.Error(), "not a git repository") || !strings.Contains(err.Error(), "in_place") {
		t.Errorf("error should name the problem and the fix, got: %v", err)
	}
}

// A repo with no commits has nothing to branch from. The message has to say so,
// because "git worktree add failed" alone sends the user hunting in the wrong
// place.
func TestPrepareLocalWorktreeRejectsRepoWithoutCommits(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")

	_, err := PrepareLocalWorktree(LocalWorktreeParams{
		LocalPath: dir,
		EnvRoot:   t.TempDir(),
		TaskID:    "task-1",
	}, worktreeTestLogger())
	if err == nil {
		t.Fatal("expected an error for a repo with no commits")
	}
	if !strings.Contains(err.Error(), "no commit") {
		t.Errorf("error should explain the missing commit, got: %v", err)
	}
}

// Concurrency is the entire point of the mode: two tasks on one directory must
// both get a working checkout, with distinct branches, without corrupting git's
// admin files.
func TestPrepareLocalWorktreeConcurrentTasks(t *testing.T) {
	repo := newTestRepo(t)

	const tasks = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*LocalWorktree
		errs    []error
	)
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wt, err := PrepareLocalWorktree(LocalWorktreeParams{
				LocalPath: repo,
				EnvRoot:   t.TempDir(),
				AgentName: "J",
				TaskID:    strings.Repeat(string(rune('a'+i)), 8),
			}, worktreeTestLogger())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, wt)
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent prepare failed: %v", err)
	}
	if len(results) != tasks {
		t.Fatalf("got %d worktrees, want %d", len(results), tasks)
	}
	branches := map[string]bool{}
	for _, wt := range results {
		if branches[wt.Branch] {
			t.Errorf("duplicate branch %q across concurrent tasks", wt.Branch)
		}
		branches[wt.Branch] = true
		if got := readFile(t, filepath.Join(wt.Path, "tracked.txt")); got != "original\n" {
			t.Errorf("worktree %s has wrong content: %q", wt.Path, got)
		}
	}
	for _, wt := range results {
		finalizeOK(t, wt)
	}
	if list := gitRun(t, repo, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Errorf("worktrees left registered after finalize:\n%s", list)
	}
}

// End-to-end shape of a worktree-mode task: Prepare builds the env and writes
// its sidecars into the worktree, the daemon's cleanup pass removes them, and
// Finalize delivers a branch carrying only the agent's real work. The
// sidecar-free branch is the user-visible contract — a diff full of
// .agent_context/ scaffolding would make the mode unusable for review.
func TestWorktreeModeDeliversBranchWithoutSidecars(t *testing.T) {
	repo := newTestRepo(t)
	// Start dirty, so the branch gets a baseline commit as well as the agent's
	// own — a sidecar could otherwise hide in either one.
	writeFile(t, filepath.Join(repo, "tracked.txt"), "edited by user\n")
	originalHead := gitRun(t, repo, "rev-parse", "HEAD")
	envRoot := filepath.Join(t.TempDir(), "env")

	env, err := Prepare(PrepareParams{
		WorkspacesRoot: filepath.Dir(envRoot),
		WorkspaceID:    "ws-1",
		TaskID:         "11112222-3333-4444-5555-666677778888",
		AgentName:      "J",
		Provider:       "claude",
		LocalWorktree:  &LocalWorktreeParams{LocalPath: repo},
		Task:           TaskContextForEnv{IssueID: "issue-1", AgentName: "J"},
	}, worktreeTestLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if env.LocalWorktree == nil {
		t.Fatal("Prepare did not build a worktree")
	}
	// The env root is ordinary daemon-owned scratch in this mode, so the GC
	// exemption meant for a user's own directory must not apply.
	if env.LocalDirectory {
		t.Error("LocalDirectory = true in worktree mode; the GC would exempt this env root forever")
	}
	if env.WorkDir != env.LocalWorktree.Path {
		t.Errorf("WorkDir = %q, want the worktree root %q", env.WorkDir, env.LocalWorktree.Path)
	}
	if _, err := os.Stat(filepath.Join(env.WorkDir, ".agent_context")); err != nil {
		t.Fatalf("precondition: Prepare should have written sidecars into the worktree: %v", err)
	}

	// The agent's actual work.
	writeFile(t, filepath.Join(env.WorkDir, "real-change.txt"), "actual work\n")

	// What the daemon runs before Finalize.
	if err := CleanupRuntimeConfig(env.WorkDir, "claude"); err != nil {
		t.Fatalf("CleanupRuntimeConfig: %v", err)
	}
	if err := CleanupSidecars(env.RootDir); err != nil {
		t.Fatalf("CleanupSidecars: %v", err)
	}
	outcome := finalizeOK(t, env.LocalWorktree)

	if outcome.Branch == "" {
		t.Fatal("no branch delivered for a task that changed a file")
	}
	// Every file the branch touches, across the baseline and agent commits.
	files := gitRun(t, repo, "diff", "--name-only", originalHead, outcome.Branch)
	if !strings.Contains(files, "real-change.txt") {
		t.Errorf("branch is missing the agent's work:\n%s", files)
	}
	for _, sidecar := range []string{".agent_context", ".multica", "CLAUDE.md"} {
		if strings.Contains(files, sidecar) {
			t.Errorf("sidecar %q leaked into the delivered branch:\n%s", sidecar, files)
		}
	}
	// The user's own checkout keeps exactly the edit it started with, and
	// nothing the agent or the runtime produced.
	if got := gitRun(t, repo, "status", "--porcelain"); got != "M tracked.txt" {
		t.Errorf("user's working tree changed, want only their own edit:\n%s", got)
	}
}

// A crashed daemon leaves a registration pointing at an env root that GC later
// deletes. The next task on the same repo must clean that up rather than
// accumulating dead entries in the user's `git worktree list` forever.
func TestPrepareLocalWorktreePrunesStaleRegistrations(t *testing.T) {
	repo := newTestRepo(t)
	orphanEnv := t.TempDir()
	orphan, err := PrepareLocalWorktree(LocalWorktreeParams{
		LocalPath: repo,
		EnvRoot:   orphanEnv,
		AgentName: "J",
		TaskID:    "dead-task",
	}, worktreeTestLogger())
	if err != nil {
		t.Fatalf("seed orphan worktree: %v", err)
	}
	// Simulate the crash: the directory disappears with GC, the registration
	// survives in the user's repo.
	if err := os.RemoveAll(orphan.Path); err != nil {
		t.Fatalf("remove orphan worktree dir: %v", err)
	}
	if list := gitRun(t, repo, "worktree", "list"); !strings.Contains(list, orphan.Path) {
		t.Fatalf("precondition failed: orphan not registered:\n%s", list)
	}

	wt := prepareForTest(t, repo)
	t.Cleanup(func() { finalizeOK(t, wt) })

	if list := gitRun(t, repo, "worktree", "list"); strings.Contains(list, orphan.Path) {
		t.Errorf("stale registration not pruned:\n%s", list)
	}
}

// finalizeOK finalizes a worktree and fails the test if the work could not be
// persisted. Tests that exercise the failure path call Finalize directly.
func finalizeOK(t *testing.T, wt *LocalWorktree) LocalWorktreeOutcome {
	t.Helper()
	outcome, err := wt.Finalize(worktreeTestLogger())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return outcome
}

// The one operation in this flow that can destroy work is `git worktree remove
// --force`. If the agent's changes could not be committed, removing the
// worktree would delete the only copy — so Finalize must keep it and say so.
// commit.gpgSign with no usable key is the realistic trigger: --no-verify does
// not disable signing.
func TestFinalizeKeepsWorktreeWhenCommitFails(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)

	writeFile(t, filepath.Join(wt.Path, "agent-output.txt"), "irreplaceable work\n")

	// Force every commit in this worktree to fail the way a signing setup the
	// daemon can't satisfy would.
	gitRun(t, wt.Path, "config", "commit.gpgSign", "true")
	gitRun(t, wt.Path, "config", "gpg.program", filepath.Join(repo, "definitely-not-a-real-gpg"))

	outcome, err := wt.Finalize(worktreeTestLogger())

	if err == nil {
		t.Fatal("Finalize returned nil error after the commit failed")
	}
	if outcome.PreservedPath != wt.Path {
		t.Errorf("PreservedPath = %q, want %q", outcome.PreservedPath, wt.Path)
	}
	// The whole point: the work is still on disk.
	if got := readFile(t, filepath.Join(wt.Path, "agent-output.txt")); got != "irreplaceable work\n" {
		t.Errorf("agent work was destroyed despite the commit failure, got %q", got)
	}
	// And it stays discoverable through git rather than only a log line.
	if list := gitRun(t, repo, "worktree", "list"); !strings.Contains(list, wt.Path) {
		t.Errorf("preserved worktree is no longer registered, so the user cannot find it:\n%s", list)
	}
	if !strings.Contains(err.Error(), wt.Path) {
		t.Errorf("error should name the preserved path, got: %v", err)
	}
}

// An in_place task on the same directory leaves .agent_context/ and .multica/
// in the user's tree while it runs. A concurrent worktree snapshot sees them as
// untracked files; copying them would hand this task another issue's brief and
// commit it to the branch.
func TestPrepareLocalWorktreeSkipsMulticaSidecars(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, ".agent_context", "issue_context.md"), "OTHER issue's brief\n")
	writeFile(t, filepath.Join(repo, ".multica", "project", "resources.json"), "{}\n")
	// An in_place resource may point at a SUBDIRECTORY of this repo, in which
	// case its sidecars sit below that subdirectory, not at the repo root.
	writeFile(t, filepath.Join(repo, "services", "api", ".agent_context", "issue_context.md"), "yet another issue's brief\n")
	writeFile(t, filepath.Join(repo, "services", "api", ".multica", "task.json"), "{}\n")
	writeFile(t, filepath.Join(repo, "real-untracked.txt"), "user's own file\n")
	writeFile(t, filepath.Join(repo, "services", "api", "notes.txt"), "user's nested file\n")

	wt := prepareForTest(t, repo)
	t.Cleanup(func() { finalizeOK(t, wt) })

	if _, err := os.Stat(filepath.Join(wt.Path, ".agent_context")); !os.IsNotExist(err) {
		t.Error("another task's .agent_context was copied into this worktree")
	}
	if _, err := os.Stat(filepath.Join(wt.Path, ".multica")); !os.IsNotExist(err) {
		t.Error("another task's .multica was copied into this worktree")
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "services", "api", ".agent_context")); !os.IsNotExist(err) {
		t.Error("a subdirectory task's .agent_context was copied into this worktree")
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "services", "api", ".multica")); !os.IsNotExist(err) {
		t.Error("a subdirectory task's .multica was copied into this worktree")
	}
	if got := readFile(t, filepath.Join(wt.Path, "real-untracked.txt")); got != "user's own file\n" {
		t.Errorf("the user's own untracked file was not replayed: %q", got)
	}
	if got := readFile(t, filepath.Join(wt.Path, "services", "api", "notes.txt")); got != "user's nested file\n" {
		t.Errorf("the user's nested untracked file was not replayed: %q", got)
	}
}

// Silently starting from HEAD when the user's uncommitted state cannot be
// replayed would have the agent review code the user never wrote and report on
// it confidently. Refusing to start is the recoverable outcome.
func TestPrepareLocalWorktreeFailsWhenUntrackedReplayIsTruncated(t *testing.T) {
	repo := newTestRepo(t)
	// One file over the copy budget is enough to prove the bound fails closed
	// rather than under-copying; writing 2001 files would only be slower.
	big := strings.Repeat("x", 1024)
	for i := range maxUntrackedFiles + 1 {
		writeFile(t, filepath.Join(repo, "untracked", "f"+string(rune('a'+i%26))+strings.Repeat("z", i/26)+".txt"), big)
	}

	_, err := PrepareLocalWorktree(LocalWorktreeParams{
		LocalPath: repo,
		EnvRoot:   t.TempDir(),
		AgentName: "J",
		TaskID:    "task-truncated",
	}, worktreeTestLogger())

	if err == nil {
		t.Fatal("expected prepare to fail rather than replay a truncated tree")
	}
	if !strings.Contains(err.Error(), "in_place") {
		t.Errorf("error should offer a way out, got: %v", err)
	}
	// A failed prepare must not leave a half-built worktree registered.
	if list := gitRun(t, repo, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Errorf("aborted prepare left a worktree registered:\n%s", list)
	}
	if branches := gitRun(t, repo, "branch", "--list", "agent/*"); branches != "" {
		t.Errorf("aborted prepare left a branch behind: %q", branches)
	}
}

// Worktree tasks get a fresh env root per task ID, exactly like in_place ones
// (shouldReusePriorWorkdir refuses any local assignment). So they need the same
// per-issue Codex session store: without it, each comment turn on an issue
// starts a new empty CODEX_HOME, the prior rollout is invisible, and the agent
// silently loses the conversation it was having with the user.
func TestPrepareWorktreeModeUsesPerIssueCodexSessionStore(t *testing.T) {
	repo := newTestRepo(t)
	workspacesRoot := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	prepareTurn := func(taskID string) *Environment {
		t.Helper()
		env, err := Prepare(PrepareParams{
			WorkspacesRoot: workspacesRoot,
			WorkspaceID:    "ws-1",
			TaskID:         taskID,
			AgentName:      "J",
			Provider:       "codex",
			LocalWorktree:  &LocalWorktreeParams{LocalPath: repo},
			Task:           TaskContextForEnv{IssueID: "issue-1", AgentID: "agent-1", AgentName: "J"},
		}, worktreeTestLogger())
		if err != nil {
			t.Fatalf("Prepare(%s): %v", taskID, err)
		}
		t.Cleanup(func() { finalizeOK(t, env.LocalWorktree) })
		return env
	}

	// Two turns on the same issue, different task IDs — the shape of a
	// follow-up comment.
	first := prepareTurn("aaaa1111-2222-3333-4444-5555666677aa")
	second := prepareTurn("bbbb1111-2222-3333-4444-5555666677bb")

	sessionsOf := func(env *Environment) string {
		t.Helper()
		target, err := filepath.EvalSymlinks(filepath.Join(env.CodexHome, "sessions"))
		if err != nil {
			t.Fatalf("resolve sessions dir: %v", err)
		}
		return target
	}

	firstSessions := sessionsOf(first)
	secondSessions := sessionsOf(second)
	if firstSessions != secondSessions {
		t.Errorf("each turn got its own sessions dir, so Codex cannot resume:\n first  %s\n second %s",
			firstSessions, secondSessions)
	}
	// And it must be the stable per-issue store, not a task-local directory.
	if strings.Contains(firstSessions, taskKey("aaaa1111-2222-3333-4444-5555666677aa")) {
		t.Errorf("sessions dir is task-scoped (%s); it will not survive the next turn", firstSessions)
	}
}

// The daemon runs its sidecar cleanup before Finalize commits. If that cleanup
// fails, committing anyway would deliver a branch whose content is Multica's
// own runtime files — the exact leak this mode promises to prevent. The abort
// must therefore stop the commit AND keep the worktree, since the agent's work
// is still in it.
func TestFinalizeAbortRefusesToCommitAndKeepsWorktree(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)

	writeFile(t, filepath.Join(wt.Path, "agent-output.txt"), "real work\n")
	writeFile(t, filepath.Join(wt.Path, ".agent_context", "issue_context.md"), "runtime brief\n")

	wt.AbortWithReason(errors.New("cleanup failed"))
	outcome, err := wt.Finalize(worktreeTestLogger())

	if err == nil {
		t.Fatal("Finalize returned nil error after an abort")
	}
	if outcome.Branch != "" {
		t.Errorf("Branch = %q, want empty: nothing may be delivered after an abort", outcome.Branch)
	}
	if outcome.AutoCommitted {
		t.Error("AutoCommitted = true; the abort must prevent the commit")
	}
	if outcome.PreservedPath != wt.Path {
		t.Errorf("PreservedPath = %q, want %q", outcome.PreservedPath, wt.Path)
	}
	// Nothing committed: the branch must still be sitting on its base.
	tip := gitRun(t, repo, "rev-parse", wt.Branch)
	if tip != wt.BaseCommit {
		t.Errorf("branch moved to %s; the sidecar-carrying commit was delivered anyway", tip)
	}
	// And the work is still recoverable on disk.
	if got := readFile(t, filepath.Join(wt.Path, "agent-output.txt")); got != "real work\n" {
		t.Errorf("agent work was destroyed: %q", got)
	}
	if list := gitRun(t, repo, "worktree", "list"); !strings.Contains(list, wt.Path) {
		t.Errorf("preserved worktree is no longer registered:\n%s", list)
	}
}

// The first reason is the one closest to the root cause, so later aborts must
// not overwrite it.
func TestAbortWithReasonKeepsFirstReason(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)
	t.Cleanup(func() { removeLocalWorktreeDir(repo, wt.Path, worktreeTestLogger()) })

	wt.AbortWithReason(errors.New("first"))
	wt.AbortWithReason(errors.New("second"))
	wt.AbortWithReason(nil)

	_, err := wt.Finalize(worktreeTestLogger())
	if err == nil || !strings.Contains(err.Error(), "first") {
		t.Fatalf("want the first reason preserved, got: %v", err)
	}
	if strings.Contains(err.Error(), "second") {
		t.Errorf("later abort overwrote the original reason: %v", err)
	}
}

// An untracked symlink is content the user can see. Replaying it faithfully is
// ambiguous (link vs target, targets outside the repo), so the snapshot must
// fail rather than hand the agent a tree with a file quietly missing.
func TestPrepareLocalWorktreeFailsOnUntrackedSymlink(t *testing.T) {
	repo := newTestRepo(t)
	if err := os.Symlink(filepath.Join(repo, "tracked.txt"), filepath.Join(repo, "shortcut.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := PrepareLocalWorktree(LocalWorktreeParams{
		LocalPath: repo,
		EnvRoot:   t.TempDir(),
		AgentName: "J",
		TaskID:    "task-symlink",
	}, worktreeTestLogger())

	if err == nil {
		t.Fatal("expected prepare to fail rather than silently drop the symlink")
	}
	if !strings.Contains(err.Error(), "untracked") {
		t.Errorf("error should name the untracked replay, got: %v", err)
	}
	if list := gitRun(t, repo, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Errorf("aborted prepare left a worktree registered:\n%s", list)
	}
}

// Discard is the abandon path for a Prepare that fails after the worktree
// exists. Without it every such failure leaves a registration in the user's
// repo and a branch no task ever ran in.
func TestDiscardRemovesWorktreeAndBranch(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)

	wt.Discard(worktreeTestLogger())

	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree directory survived Discard: %v", err)
	}
	if list := gitRun(t, repo, "worktree", "list"); strings.Contains(list, wt.Path) {
		t.Errorf("worktree still registered after Discard:\n%s", list)
	}
	if out, err := gitTry(t, repo, "rev-parse", "--verify", wt.Branch); err == nil {
		t.Errorf("branch survived Discard, resolves to %s", out)
	}
}

// prepareTurn runs one turn of a conversation: a fresh task id and env root on
// the same conversation key, which is what a follow-up comment on an issue
// produces.
func prepareTurn(t *testing.T, localPath, conversationKey, taskID string) *LocalWorktree {
	t.Helper()
	wt, err := PrepareLocalWorktree(LocalWorktreeParams{
		LocalPath:       localPath,
		EnvRoot:         t.TempDir(),
		AgentName:       "J",
		TaskID:          taskID,
		ConversationKey: conversationKey,
	}, worktreeTestLogger())
	if err != nil {
		t.Fatalf("PrepareLocalWorktree(%s): %v", taskID, err)
	}
	return wt
}

const (
	turnOneTask   = "11112222-3333-4444-5555-aaaaaaaaaaaa"
	turnTwoTask   = "11112222-3333-4444-5555-bbbbbbbbbbbb"
	turnThreeTask = "11112222-3333-4444-5555-cccccccccccc"
)

// The bug this fixes: every comment on one issue produced a new branch forked
// from HEAD, so the second turn stood in a tree that did not contain the first
// turn's work and nothing said so (MUL-6881).
func TestPrepareLocalWorktreeContinuesTheConversationBranch(t *testing.T) {
	repo := newTestRepo(t)

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	if first.Branch != "agent/j/mul-6881" {
		t.Fatalf("first turn branch = %q, want agent/j/mul-6881", first.Branch)
	}
	if first.Continued {
		t.Error("first turn reports Continued = true; there was nothing to continue")
	}
	writeFile(t, filepath.Join(first.WorkDir, "turn-one.txt"), "work from turn one\n")
	firstOutcome := finalizeOK(t, first)
	if firstOutcome.Branch != "agent/j/mul-6881" {
		t.Fatalf("first outcome branch = %q", firstOutcome.Branch)
	}
	firstTip := gitRun(t, repo, "rev-parse", "agent/j/mul-6881")

	second := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	if second.Branch != "agent/j/mul-6881" {
		t.Fatalf("second turn branch = %q, want the conversation's branch", second.Branch)
	}
	if !second.Continued {
		t.Error("second turn reports Continued = false, want true")
	}
	if second.BaseCommit != firstTip {
		t.Errorf("second turn base = %s, want the first turn's tip %s", second.BaseCommit, firstTip)
	}
	if got := readFile(t, filepath.Join(second.WorkDir, "turn-one.txt")); got != "work from turn one\n" {
		t.Errorf("turn one's work is missing from turn two's tree: %q", got)
	}

	writeFile(t, filepath.Join(second.WorkDir, "turn-two.txt"), "work from turn two\n")
	finalizeOK(t, second)

	third := prepareTurn(t, repo, "MUL-6881", turnThreeTask)
	for _, name := range []string{"turn-one.txt", "turn-two.txt"} {
		if _, err := os.Stat(filepath.Join(third.WorkDir, name)); err != nil {
			t.Errorf("turn three is missing %s: %v", name, err)
		}
	}
	finalizeOK(t, third)

	// One branch for the whole conversation, not one per comment.
	branches := gitRun(t, repo, "branch", "--list", "agent/j/*")
	if strings.Count(branches, "agent/j/") != 1 {
		t.Errorf("conversation produced more than one branch:\n%s", branches)
	}
}

// The user's uncommitted work reaches the branch once, as turn one's baseline.
// Replaying their whole tree again on every turn would ask git to merge their
// copy of a file against the agent's edits to it — a conflict raised precisely
// when the agent did what it was asked to do — so only what they changed since
// is replayed.
func TestPrepareLocalWorktreeReplaysOnlyTheUserEditsSinceTheLastTurn(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "user work in progress\n")

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	if got := readFile(t, filepath.Join(first.WorkDir, "tracked.txt")); got != "user work in progress\n" {
		t.Fatalf("turn one did not see the user's uncommitted work: %q", got)
	}
	// The agent edits the very line the user was working on.
	writeFile(t, filepath.Join(first.WorkDir, "tracked.txt"), "user work in progress, finished by the agent\n")
	finalizeOK(t, first)

	// Between the turns the user edits a different file and leaves the first
	// one exactly as it was.
	writeFile(t, filepath.Join(repo, "keep.txt"), "user edited this between turns\n")

	second := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	if got := readFile(t, filepath.Join(second.WorkDir, "tracked.txt")); got != "user work in progress, finished by the agent\n" {
		t.Errorf("the agent's edit was reverted by replaying the user's older copy: %q", got)
	}
	if got := readFile(t, filepath.Join(second.WorkDir, "keep.txt")); got != "user edited this between turns\n" {
		t.Errorf("the user's edit between turns did not reach the worktree: %q", got)
	}
	if strings.Contains(readFile(t, filepath.Join(second.WorkDir, "tracked.txt")), "<<<<<<<") {
		t.Error("the worktree was handed to the agent with conflict markers in it")
	}
	// The user's own directory is never written to, on any turn.
	if got := readFile(t, filepath.Join(repo, "tracked.txt")); got != "user work in progress\n" {
		t.Errorf("the user's directory was modified: %q", got)
	}
}

// A real conflict — the user rewrites lines the agent also rewrote — must not
// strand the conversation. The turn runs on the branch as it stands, which is
// the tree the agent worked in last time.
func TestPrepareLocalWorktreeSurvivesConflictingUserEdits(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "user work in progress\n")

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	writeFile(t, filepath.Join(first.WorkDir, "tracked.txt"), "rewritten by the agent\n")
	finalizeOK(t, first)

	// The user rewrites the same line their own way.
	writeFile(t, filepath.Join(repo, "tracked.txt"), "rewritten by the user instead\n")

	second := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	got := readFile(t, filepath.Join(second.WorkDir, "tracked.txt"))
	if got != "rewritten by the agent\n" {
		t.Errorf("tracked.txt = %q, want the branch's own version", got)
	}
	if status := gitRun(t, second.Path, "status", "--porcelain"); status != "" {
		t.Errorf("worktree left dirty after a conflicting replay:\n%s", status)
	}
}

// An untracked file becomes part of the branch at the turn that first sees it.
// Copying the user's older copy over it on the next turn would revert whatever
// the agent did to it — silently, every turn.
func TestPrepareLocalWorktreeKeepsAgentEditsToOnceUntrackedFiles(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, filepath.Join(repo, "scratch.txt"), "user scratch\n")

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	writeFile(t, filepath.Join(first.WorkDir, "scratch.txt"), "scratch, rewritten by the agent\n")
	finalizeOK(t, first)

	second := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	if got := readFile(t, filepath.Join(second.WorkDir, "scratch.txt")); got != "scratch, rewritten by the agent\n" {
		t.Errorf("scratch.txt = %q, want the agent's version", got)
	}
}

// A turn that only reads must leave the conversation's branch alone: it holds
// every turn before it, and the read-only cleanup would take those with it.
func TestFinalizeKeepsTheConversationBranchAfterAReadOnlyTurn(t *testing.T) {
	repo := newTestRepo(t)

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	writeFile(t, filepath.Join(first.WorkDir, "turn-one.txt"), "work from turn one\n")
	finalizeOK(t, first)

	second := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	outcome := finalizeOK(t, second)
	if outcome.Branch != "agent/j/mul-6881" {
		t.Errorf("read-only turn reported branch %q, want the conversation's branch", outcome.Branch)
	}
	if _, err := gitTry(t, repo, "rev-parse", "--verify", "agent/j/mul-6881"); err != nil {
		t.Fatal("the conversation's branch was deleted by a turn that changed nothing")
	}
	if got := gitRun(t, repo, "show", "agent/j/mul-6881:turn-one.txt"); got != "work from turn one" {
		t.Errorf("turn one's work is gone from the branch: %q", got)
	}
}

// Discard runs when preparation fails after the worktree exists. It may only
// drop a branch this prepare created.
func TestDiscardKeepsTheContinuedBranch(t *testing.T) {
	repo := newTestRepo(t)

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	writeFile(t, filepath.Join(first.WorkDir, "turn-one.txt"), "work from turn one\n")
	finalizeOK(t, first)

	second := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	second.Discard(worktreeTestLogger())

	if _, err := gitTry(t, repo, "rev-parse", "--verify", "agent/j/mul-6881"); err != nil {
		t.Fatal("discarding a follow-up turn deleted the conversation's branch")
	}
	if _, err := os.Stat(second.Path); !os.IsNotExist(err) {
		t.Errorf("worktree still on disk after Discard: %v", err)
	}
}

// git allows one worktree per branch. A sibling task on the same conversation
// still has to run, and it should start from the conversation's latest work
// rather than from HEAD.
func TestPrepareLocalWorktreeForksWhenTheConversationBranchIsBusy(t *testing.T) {
	repo := newTestRepo(t)

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	writeFile(t, filepath.Join(first.WorkDir, "turn-one.txt"), "work from turn one\n")
	// Commit inside the worktree so the sibling has something to inherit while
	// the branch is still checked out here.
	gitRun(t, first.Path, "add", "-A")
	gitRun(t, first.Path, "commit", "-m", "turn one")
	tip := gitRun(t, repo, "rev-parse", "agent/j/mul-6881")

	sibling := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	if sibling.Branch == first.Branch {
		t.Fatalf("sibling took the branch already checked out at %s", first.Path)
	}
	if want := "agent/j/mul-6881-" + taskKey(turnTwoTask); sibling.Branch != want {
		t.Errorf("sibling branch = %q, want %q", sibling.Branch, want)
	}
	if sibling.BaseCommit != tip {
		t.Errorf("sibling base = %s, want the conversation's tip %s", sibling.BaseCommit, tip)
	}
	if _, err := os.Stat(filepath.Join(sibling.WorkDir, "turn-one.txt")); err != nil {
		t.Errorf("sibling does not carry the conversation's work: %v", err)
	}
	finalizeOK(t, sibling)
	finalizeOK(t, first)
}

// Once the user merges the branch, its tip carries nothing HEAD does not.
// Continuing from it would leave the next turn behind the user's own commits.
func TestPrepareLocalWorktreeRestartsAMergedConversationBranch(t *testing.T) {
	repo := newTestRepo(t)

	first := prepareTurn(t, repo, "MUL-6881", turnOneTask)
	writeFile(t, filepath.Join(first.WorkDir, "turn-one.txt"), "work from turn one\n")
	finalizeOK(t, first)

	gitRun(t, repo, "merge", "--no-edit", "agent/j/mul-6881")
	writeFile(t, filepath.Join(repo, "user-commit.txt"), "the user kept working\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-m", "user work after the merge")
	head := gitRun(t, repo, "rev-parse", "HEAD")

	second := prepareTurn(t, repo, "MUL-6881", turnTwoTask)
	if second.Continued {
		t.Error("continued a branch the user had already merged")
	}
	if second.BaseCommit != head {
		t.Errorf("second turn base = %s, want the user's HEAD %s", second.BaseCommit, head)
	}
	if _, err := os.Stat(filepath.Join(second.WorkDir, "user-commit.txt")); err != nil {
		t.Errorf("the user's commits after the merge are missing: %v", err)
	}
}

// A task with no conversation behind it — no issue, no chat session — has
// nothing to continue and keeps the task-scoped branch.
func TestPrepareLocalWorktreeKeepsTaskScopedBranchWithoutAConversation(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)
	if want := "agent/j/" + taskKey("11112222-3333-4444-5555-666677778888"); wt.Branch != want {
		t.Errorf("branch = %q, want %q", wt.Branch, want)
	}
	if wt.Continued {
		t.Error("a task with no conversation reports Continued = true")
	}
}

func TestLocalWorktreeConversationKey(t *testing.T) {
	issueID := "01a056ac-0eda-797d-8ac2-b7d7a3935ae7"
	chatID := "01a056ad-5b37-762a-a15f-390717f4dae1"
	tests := []struct {
		name   string
		params PrepareParams
		want   string
	}{
		{"issue identifier is preferred", PrepareParams{IssueIdentifier: "MUL-6881", Task: TaskContextForEnv{IssueID: issueID}}, "MUL-6881"},
		{"issue without an identifier", PrepareParams{Task: TaskContextForEnv{IssueID: issueID}}, taskKey(issueID)},
		{"chat session", PrepareParams{Task: TaskContextForEnv{ChatSessionID: chatID}}, "chat-" + taskKey(chatID)},
		{"neither", PrepareParams{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := localWorktreeConversationKey(tt.params); got != tt.want {
				t.Errorf("localWorktreeConversationKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
