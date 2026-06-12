package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pull tests run entirely against on-disk repos: the package init()
// installs go-git's in-process server for the "file" protocol, so a
// plain local path works as a remote without any system git.

// seedOrigin creates a repo at dir with one commit containing files.
func seedOrigin(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init origin: %v", err)
	}
	commitFiles(t, dir, files, "initial commit")
}

// commitFiles writes files, stages everything, and commits.
func commitFiles(t *testing.T, dir string, files map[string]string, msg string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, err := Add(AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := Commit(CommitOptions{RepoPath: dir, Message: msg, AuthorName: "T", AuthorEmail: "t@e"}); err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
}

// cloneTo clones origin into a fresh tempdir and returns it.
func cloneTo(t *testing.T, origin string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	if _, err := Clone(CloneOptions{URL: origin, Path: dir}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	return dir
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestPullFastForwardClean(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n"})
	clone := cloneTo(t, origin)

	commitFiles(t, origin, map[string]string{"a.txt": "v2\n", "new.txt": "hello\n"}, "advance")

	res, err := Pull(PullOptions{RepoPath: clone})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(res.Message, "Fast-forward") {
		t.Errorf("expected Fast-forward message, got %q", res.Message)
	}
	if got := readFile(t, clone, "a.txt"); got != "v2\n" {
		t.Errorf("a.txt not updated: %q", got)
	}
	if got := readFile(t, clone, "new.txt"); got != "hello\n" {
		t.Errorf("new.txt missing/wrong: %q", got)
	}
	_, logs, err := Log(LogOptions{RepoPath: clone, Number: 10})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 commits after pull, got %d", len(logs))
	}
	// Tree must be clean after a clean fast-forward.
	_, entries, err := Status(clone)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if entries != nil {
		t.Errorf("expected clean tree after pull, got %v", entries)
	}
}

func TestPullAlreadyUpToDate(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n"})
	clone := cloneTo(t, origin)

	res, err := Pull(PullOptions{RepoPath: clone})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(res.Message, "Already up to date.") {
		t.Errorf("expected up-to-date message, got %q", res.Message)
	}
}

// TestPullWhenAhead is the regression test for the original bug:
// go-git's Worktree.Pull returns "non-fast-forward update" when the
// local branch is simply ahead. Real git — and now outpost git —
// reports already-up-to-date.
func TestPullWhenAhead(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n"})
	clone := cloneTo(t, origin)

	commitFiles(t, clone, map[string]string{"local.txt": "mine\n"}, "local work")

	res, err := Pull(PullOptions{RepoPath: clone})
	if err != nil {
		t.Fatalf("pull on ahead branch: %v", err)
	}
	if !strings.Contains(res.Message, "Already up to date.") {
		t.Errorf("expected up-to-date message, got %q", res.Message)
	}
	if !strings.Contains(res.Message, "ahead") {
		t.Errorf("expected ahead-of-remote hint, got %q", res.Message)
	}
}

func TestPullDiverged(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n"})
	clone := cloneTo(t, origin)

	commitFiles(t, origin, map[string]string{"a.txt": "remote\n"}, "remote work")
	commitFiles(t, clone, map[string]string{"local.txt": "mine\n"}, "local work")

	_, err := Pull(PullOptions{RepoPath: clone})
	if err == nil {
		t.Fatalf("expected divergence error")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("expected diverged in error, got %v", err)
	}
}

// TestPullDirtyNonConflicting: real `git pull` succeeds when local
// uncommitted changes don't overlap the incoming commits. go-git's
// Worktree.Pull refuses; ours must succeed and preserve the local
// modification and untracked files.
func TestPullDirtyNonConflicting(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n", "b.txt": "b1\n"})
	clone := cloneTo(t, origin)

	commitFiles(t, origin, map[string]string{"a.txt": "v2\n", "c.txt": "c1\n"}, "advance")

	// Local, uncommitted edit to an unrelated file + an untracked file.
	if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, "untracked.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	res, err := Pull(PullOptions{RepoPath: clone})
	if err != nil {
		t.Fatalf("pull with non-conflicting dirty tree: %v", err)
	}
	if !strings.Contains(res.Message, "Fast-forward") {
		t.Errorf("expected Fast-forward, got %q", res.Message)
	}
	if got := readFile(t, clone, "a.txt"); got != "v2\n" {
		t.Errorf("a.txt not updated: %q", got)
	}
	if got := readFile(t, clone, "c.txt"); got != "c1\n" {
		t.Errorf("c.txt missing: %q", got)
	}
	if got := readFile(t, clone, "b.txt"); got != "local edit\n" {
		t.Errorf("local edit to b.txt lost: %q", got)
	}
	if got := readFile(t, clone, "untracked.txt"); got != "keep me\n" {
		t.Errorf("untracked file lost: %q", got)
	}
	// b.txt must still show as modified, untracked.txt as untracked.
	_, entries, err := Status(clone)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var sawB, sawUntracked bool
	for _, e := range entries {
		switch e.File {
		case "b.txt":
			sawB = true
		case "untracked.txt":
			sawUntracked = true
		case "a.txt", "c.txt":
			t.Errorf("pulled file %s shows dirty: %+v", e.File, e)
		}
	}
	if !sawB || !sawUntracked {
		t.Errorf("expected b.txt + untracked.txt in status, got %v", entries)
	}
	// Log advanced to the remote tip.
	_, logs, err := Log(LogOptions{RepoPath: clone, Number: 10})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 commits, got %d", len(logs))
	}
}

func TestPullDirtyConflicting(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n"})
	clone := cloneTo(t, origin)

	commitFiles(t, origin, map[string]string{"a.txt": "remote v2\n"}, "advance")
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Pull(PullOptions{RepoPath: clone})
	if err == nil {
		t.Fatalf("expected overwrite error")
	}
	if !strings.Contains(err.Error(), "a.txt") || !strings.Contains(err.Error(), "overwritten") {
		t.Errorf("expected git-style overwrite error naming a.txt, got %v", err)
	}
	// Nothing may have been mutated: local edit intact, HEAD unmoved.
	if got := readFile(t, clone, "a.txt"); got != "local edit\n" {
		t.Errorf("local edit clobbered: %q", got)
	}
	_, logs, err := Log(LogOptions{RepoPath: clone, Number: 10})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("HEAD moved despite aborted pull: %d commits", len(logs))
	}
}

// TestPullOnNewBranchErrs: the original wrapper silently integrated
// the remote's default branch when run from any branch. Now a branch
// with no upstream and no same-name remote branch is a clear error.
func TestPullOnNewBranchErrs(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n"})
	clone := cloneTo(t, origin)

	if _, err := Checkout(CheckoutOptions{RepoPath: clone, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, origin, map[string]string{"a.txt": "v2\n"}, "advance")

	_, err := Pull(PullOptions{RepoPath: clone})
	if err == nil {
		t.Fatalf("expected error pulling a branch with no remote counterpart")
	}
	if !strings.Contains(err.Error(), "feature") {
		t.Errorf("error should name the missing branch, got %v", err)
	}
	// File must NOT have been updated from the remote default branch.
	if got := readFile(t, clone, "a.txt"); got != "v1\n" {
		t.Errorf("pull on feature branch integrated the default branch: %q", got)
	}
}

// TestPullExplicitBranch: `outpost git pull origin <branch>` integrates
// the named branch into the current one when fast-forward is possible.
func TestPullExplicitBranch(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	seedOrigin(t, origin, map[string]string{"a.txt": "v1\n"})
	clone := cloneTo(t, origin)

	// Find origin's default branch name (what clone checked out).
	rp, err := RevParse(RevParseOptions{RepoPath: clone})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	_, branches, err := Branch(BranchOptions{RepoPath: clone})
	if err != nil || len(branches) == 0 {
		t.Fatalf("branch list: %v %v", branches, err)
	}
	var defBranch string
	for _, b := range branches {
		if strings.HasPrefix(b, "* ") {
			defBranch = strings.TrimPrefix(b, "* ")
		}
	}
	if defBranch == "" {
		t.Fatalf("no current branch in %v", branches)
	}

	if _, err := Checkout(CheckoutOptions{RepoPath: clone, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, origin, map[string]string{"a.txt": "v2\n"}, "advance")

	res, err := Pull(PullOptions{RepoPath: clone, Remote: "origin", Branch: defBranch})
	if err != nil {
		t.Fatalf("pull origin %s: %v", defBranch, err)
	}
	if !strings.Contains(res.Message, "Fast-forward") {
		t.Errorf("expected Fast-forward, got %q", res.Message)
	}
	if got := readFile(t, clone, "a.txt"); got != "v2\n" {
		t.Errorf("a.txt not updated: %q", got)
	}
	rp2, err := RevParse(RevParseOptions{RepoPath: clone})
	if err != nil {
		t.Fatalf("rev-parse after pull: %v", err)
	}
	if rp2.Hash == rp.Hash {
		t.Errorf("feature branch did not advance")
	}
}
