package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitCommandWiring exercises the cobra wiring end-to-end against
// an on-disk tempdir repo. The library logic is covered in
// internal/agent/git/git_test.go; this test catches drift between the
// cobra flags / arg shapes and the library API.
func TestGitCommandWiring(t *testing.T) {
	dir := t.TempDir()

	// Construct the top-level command once; reuse for each invocation.
	// We need a fresh tree per invocation because cobra mutates the
	// captured-flag closures, but for these tests we use distinct
	// subtrees so no state leaks.
	run := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		root := gitCmd()
		buf := &bytes.Buffer{}
		root.SetOut(buf)
		root.SetErr(buf)
		root.SetArgs(args)
		err := root.Execute()
		return buf.String(), err
	}

	// init
	out, err := run(t, "init", dir)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf(".git missing after init: %v", err)
	}

	// Write a file directly; the `add` subcommand resolves paths
	// relative to cwd, so we cd into the tempdir for the rest.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	// add a.txt
	if out, err := run(t, "add", "a.txt"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}

	// status — should not be clean
	out, err = run(t, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Changes to be committed") {
		t.Errorf("expected staged-changes header, got:\n%s", out)
	}

	// commit (author flags so we don't need host git config)
	if out, err := run(t,
		"commit",
		"-m", "first",
		"--author-name", "T U",
		"--author-email", "tu@example.com",
	); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	// status — clean
	out, err = run(t, "status")
	if err != nil {
		t.Fatalf("status post-commit: %v\n%s", err, out)
	}
	if !strings.Contains(out, "clean") {
		t.Errorf("expected clean message, got:\n%s", out)
	}

	// log
	out, err = run(t, "log", "-n", "5")
	if err != nil {
		t.Fatalf("log: %v\n%s", err, out)
	}
	if !strings.Contains(out, "first") {
		t.Errorf("log missing commit message, got:\n%s", out)
	}

	// branch (list)
	out, err = run(t, "branch")
	if err != nil {
		t.Fatalf("branch list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "* ") {
		t.Errorf("branch list missing current marker, got:\n%s", out)
	}

	// checkout -b feature
	if out, err := run(t, "checkout", "-b", "feature"); err != nil {
		t.Fatalf("checkout -b: %v\n%s", err, out)
	}

	// show HEAD
	out, err = run(t, "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "commit ") || !strings.Contains(out, "Author: T U") {
		t.Errorf("show output malformed:\n%s", out)
	}

	// remote (empty)
	out, err = run(t, "remote")
	if err != nil {
		t.Fatalf("remote: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty remote list, got:\n%s", out)
	}

	// diff (no changes)
	out, err = run(t, "diff")
	if err != nil {
		t.Fatalf("diff: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no changes") {
		t.Errorf("expected 'no changes' for clean tree, got:\n%s", out)
	}
}

// TestGitAddRequiresPathOrAllFlag verifies that `outpost git add` with
// no args and no -A errors out cleanly instead of silently doing
// nothing.
func TestGitAddRequiresPathOrAllFlag(t *testing.T) {
	dir := t.TempDir()
	root := gitCmd()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"init", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("init: %v\n%s", err, buf.String())
	}

	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	root = gitCmd()
	buf.Reset()
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"add"})
	if err := root.Execute(); err == nil {
		t.Errorf("expected error for bare 'add', got nil")
	}
}
