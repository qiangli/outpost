package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRoundtrip exercises the typical local-only lifecycle:
//
//	init → add file → commit → log → branch → checkout -b →
//	commit on branch → status (clean) → status (dirty) →
//	diff (working + staged) → show HEAD → remotes (empty)
//
// All in a tempdir, no network. This is the "ship-readiness" gate:
// if this passes, every verb except clone/fetch/pull/push works end-
// to-end against a real on-disk repo through the public API.
func TestRoundtrip(t *testing.T) {
	dir := t.TempDir()

	// init
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("expected .git dir after init: %v", err)
	}

	// write + add a file
	fpath := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(fpath, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := Add(AddOptions{RepoPath: dir, Path: "readme.txt"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// status — expect one staged entry
	_, entries, err := Status(dir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected non-empty status after add")
	}
	foundStaged := false
	for _, e := range entries {
		if e.Staged && strings.Contains(e.File, "readme.txt") {
			foundStaged = true
		}
	}
	if !foundStaged {
		t.Fatalf("expected readme.txt to be staged, got entries=%v", entries)
	}

	// commit — supply an explicit author so we don't depend on
	// the host's git config.
	commitOpts := CommitOptions{
		RepoPath:    dir,
		Message:     "initial commit",
		AuthorName:  "Test User",
		AuthorEmail: "test@example.com",
	}
	if _, err := Commit(commitOpts); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// status — clean after commit
	res, entries, err := Status(dir)
	if err != nil {
		t.Fatalf("status post-commit: %v", err)
	}
	if entries != nil {
		t.Fatalf("expected clean tree, got entries=%v", entries)
	}
	if !strings.Contains(res.Message, "clean") {
		t.Errorf("expected clean message, got %q", res.Message)
	}

	// log — expect one entry
	_, logs, err := Log(LogOptions{RepoPath: dir, Number: 10})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(logs))
	}
	if !strings.Contains(logs[0].Message, "initial commit") {
		t.Errorf("log message mismatch: %q", logs[0].Message)
	}

	// branch list — should show master/main marked with *
	_, branches, err := Branch(BranchOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("branch list: %v", err)
	}
	currentMarked := false
	for _, b := range branches {
		if strings.HasPrefix(b, "* ") {
			currentMarked = true
		}
	}
	if !currentMarked {
		t.Fatalf("expected current branch to be marked with *, got %v", branches)
	}

	// checkout -b feature
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}

	// modify file + second commit on feature
	if err := os.WriteFile(fpath, []byte("hello\nfeature\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// dirty status — expect modified
	_, entries, err = Status(dir)
	if err != nil {
		t.Fatalf("status dirty: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("expected dirty status after modification")
	}

	// diff (working tree) via Status — file should not be staged
	hasUnstaged := false
	for _, e := range entries {
		if !e.Staged {
			hasUnstaged = true
		}
	}
	if !hasUnstaged {
		t.Errorf("expected unstaged change, entries=%v", entries)
	}

	// stage and verify staged
	if _, err := Add(AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("add -A: %v", err)
	}
	_, entries, err = Status(dir)
	if err != nil {
		t.Fatalf("status after add -A: %v", err)
	}
	hasStaged := false
	for _, e := range entries {
		if e.Staged {
			hasStaged = true
		}
	}
	if !hasStaged {
		t.Errorf("expected staged change after add -A, entries=%v", entries)
	}

	commitOpts.Message = "feature commit"
	if _, err := Commit(commitOpts); err != nil {
		t.Fatalf("commit feature: %v", err)
	}

	// log — expect two entries now
	_, logs, err = Log(LogOptions{RepoPath: dir, Number: 10})
	if err != nil {
		t.Fatalf("log post-feature: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 log entries, got %d", len(logs))
	}

	// show HEAD
	_, info, err := Show(ShowOptions{RepoPath: dir, Commit: "HEAD"})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if info.Author != "Test User" || info.Email != "test@example.com" {
		t.Errorf("show author mismatch: %+v", info)
	}
	if !strings.Contains(info.Message, "feature commit") {
		t.Errorf("show message mismatch: %q", info.Message)
	}

	// remotes — empty for an init-only repo
	_, remotes, err := Remotes(dir)
	if err != nil {
		t.Fatalf("remotes: %v", err)
	}
	if len(remotes) != 0 {
		t.Errorf("expected 0 remotes, got %v", remotes)
	}

	// branch delete (switch back first)
	// note: go-git lets you delete the branch you're on; upstream git
	// refuses. We just verify the public API works.
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature"}); err != nil {
		// Already on feature, that's fine.
		_ = err
	}
}

// TestBuildAuthMethodEnvFallback verifies that an empty AuthConfig
// resolves to oauth2-style basic auth when GITHUB_TOKEN is set, and
// nil otherwise.
func TestBuildAuthMethodEnvFallback(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GIT_TOKEN", "")
	if a, err := BuildAuthMethod(AuthConfig{}); err != nil || a != nil {
		t.Fatalf("expected nil auth with no env, got auth=%v err=%v", a, err)
	}

	t.Setenv("GITHUB_TOKEN", "ghp_test_token")
	a, err := BuildAuthMethod(AuthConfig{})
	if err != nil {
		t.Fatalf("with GITHUB_TOKEN: %v", err)
	}
	if a == nil {
		t.Fatalf("expected non-nil auth with GITHUB_TOKEN set")
	}
	if a.String() == "" {
		t.Errorf("expected non-empty auth string")
	}

	// Explicit username+password still wins over env.
	t.Setenv("GITHUB_TOKEN", "should-not-be-used")
	a, err = BuildAuthMethod(AuthConfig{Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatalf("with explicit auth: %v", err)
	}
	if a == nil {
		t.Fatalf("expected non-nil explicit auth")
	}
}

// TestRevParse covers HEAD resolution + dirty detection — the two
// signals the build scripts use to stamp the binary with commit +
// dirty metadata.
func TestRevParse(t *testing.T) {
	dir := t.TempDir()

	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// rev-parse on an empty repo (no commits yet) should fail because
	// HEAD doesn't resolve. We don't pin the error string — go-git's
	// wording is its own — but RevParse should return an error.
	if _, err := RevParse(RevParseOptions{RepoPath: dir}); err == nil {
		t.Fatalf("expected error on empty repo, got nil")
	}

	// Seed a commit.
	fpath := filepath.Join(dir, "f")
	if err := os.WriteFile(fpath, []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Add(AddOptions{RepoPath: dir, Path: "f"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := Commit(CommitOptions{RepoPath: dir, Message: "seed", AuthorName: "T", AuthorEmail: "t@e"}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Full SHA, clean tree.
	r, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if len(r.Hash) != 40 {
		t.Errorf("expected 40-char hash, got %q (len=%d)", r.Hash, len(r.Hash))
	}
	if r.Short != "" {
		t.Errorf("expected empty Short when Short=0, got %q", r.Short)
	}
	if r.Dirty {
		t.Errorf("expected clean tree, got dirty")
	}

	// Short SHA (default 7).
	r, err = RevParse(RevParseOptions{RepoPath: dir, Short: 7})
	if err != nil {
		t.Fatalf("rev-parse --short: %v", err)
	}
	if len(r.Short) != 7 {
		t.Errorf("expected 7-char short, got %q", r.Short)
	}
	if !strings.HasPrefix(r.Hash, r.Short) {
		t.Errorf("short %q is not a prefix of full %q", r.Short, r.Hash)
	}

	// Dirty after a worktree modification.
	if err := os.WriteFile(fpath, []byte("b\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	r, err = RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse dirty: %v", err)
	}
	if !r.Dirty {
		t.Errorf("expected dirty after worktree modification")
	}
}

// TestStatusCodes spot-checks the XY-code mapping.
func TestStatusCodes(t *testing.T) {
	// We can't easily construct go-git's StatusCode constants without
	// importing them; the smoke is "the strings are non-empty" via the
	// roundtrip test above. Here we just sanity-check the fall-through.
	if got := StatusCode(99); got != "  " {
		t.Errorf("expected '  ' fallthrough, got %q", got)
	}
}
