package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// isolateGlobalConfig points HOME/XDG at an empty tempdir so the
// developer's real ~/.gitconfig (user.name etc.) can't leak into
// assertions about unset identity.
func isolateGlobalConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows os.UserHomeDir
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// makeTwoCommitRepo seeds a repo with two commits on the default
// branch: a.txt at "line1\n" then "line1\nline2\n".
func makeTwoCommitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	commitFiles(t, dir, map[string]string{"a.txt": "line1\n"}, "first")
	commitFiles(t, dir, map[string]string{"a.txt": "line1\nline2\n"}, "second")
	return dir
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	_, branches, err := Branch(BranchOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("branch list: %v", err)
	}
	for _, b := range branches {
		if rest, ok := strings.CutPrefix(b, "* "); ok {
			return rest
		}
	}
	t.Fatalf("no current branch in %v", branches)
	return ""
}

func TestMergeFastForward(t *testing.T) {
	dir := makeTwoCommitRepo(t)
	main := currentBranch(t, dir)

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"feat.txt": "feature\n"}, "feature work")
	featTip, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout %s: %v", main, err)
	}
	res, err := Merge(MergeOptions{RepoPath: dir, Ref: "feature"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !strings.Contains(res.Message, "Fast-forward") {
		t.Errorf("expected Fast-forward, got %q", res.Message)
	}
	tip, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if tip.Hash != featTip.Hash {
		t.Errorf("main not fast-forwarded: %s != %s", tip.Hash, featTip.Hash)
	}
	if got := readFile(t, dir, "feat.txt"); got != "feature\n" {
		t.Errorf("worktree not updated: %q", got)
	}

	// Merging the same ref again is a no-op.
	res, err = Merge(MergeOptions{RepoPath: dir, Ref: "feature"})
	if err != nil || !strings.Contains(res.Message, "Already up to date.") {
		t.Errorf("expected up-to-date, got %v %v", res, err)
	}
}

func TestMergeNoFF(t *testing.T) {
	dir := makeTwoCommitRepo(t)
	main := currentBranch(t, dir)

	// --no-ff records a merge commit, which needs an identity.
	if _, err := ConfigSet(dir, "user.name", "Test User", false, false); err != nil {
		t.Fatalf("config user.name: %v", err)
	}
	if _, err := ConfigSet(dir, "user.email", "test@example.com", false, false); err != nil {
		t.Fatalf("config user.email: %v", err)
	}

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"feat.txt": "feature\n"}, "feature work")
	featTip, _ := RevParse(RevParseOptions{RepoPath: dir})

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout %s: %v", main, err)
	}
	preMerge, _ := RevParse(RevParseOptions{RepoPath: dir})

	res, err := Merge(MergeOptions{RepoPath: dir, Ref: "feature", NoFF: true})
	if err != nil {
		t.Fatalf("merge --no-ff: %v", err)
	}
	if !strings.Contains(res.Message, "merge commit") {
		t.Errorf("expected merge-commit message, got %q", res.Message)
	}

	r, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	commit, err := r.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("HEAD commit: %v", err)
	}
	if commit.NumParents() != 2 {
		t.Fatalf("expected 2 parents on merge commit, got %d", commit.NumParents())
	}
	p0, _ := commit.Parent(0)
	p1, _ := commit.Parent(1)
	if p0.Hash.String() != preMerge.Hash || p1.Hash.String() != featTip.Hash {
		t.Errorf("merge parents wrong: %s %s (want %s %s)", p0.Hash, p1.Hash, preMerge.Hash, featTip.Hash)
	}
	// Tree must contain the feature change.
	if got := readFile(t, dir, "feat.txt"); got != "feature\n" {
		t.Errorf("merge tree missing feature change: %q", got)
	}
}

func TestMergeDiverged(t *testing.T) {
	dir := makeTwoCommitRepo(t)
	main := currentBranch(t, dir)

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"feat.txt": "feature\n"}, "feature work")
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	commitFiles(t, dir, map[string]string{"main.txt": "main\n"}, "main work")

	if _, err := Merge(MergeOptions{RepoPath: dir, Ref: "feature"}); err == nil {
		t.Fatalf("expected divergence error")
	} else if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("expected diverged in error, got %v", err)
	}
}

func TestConfigGetSetUnset(t *testing.T) {
	isolateGlobalConfig(t)
	dir := makeTwoCommitRepo(t)

	if _, found, err := ConfigGet(dir, "user.name"); err != nil || found {
		t.Fatalf("expected unset user.name, got found=%v err=%v", found, err)
	}
	if _, err := ConfigSet(dir, "user.name", "Alice", false, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, found, err := ConfigGet(dir, "user.name")
	if err != nil || !found || v != "Alice" {
		t.Fatalf("get: %q found=%v err=%v", v, found, err)
	}

	// Arbitrary section and a subsection key both round-trip.
	if _, err := ConfigSet(dir, "outpost.testkey", "42", false, false); err != nil {
		t.Fatalf("set custom: %v", err)
	}
	if v, found, _ := ConfigGet(dir, "outpost.testkey"); !found || v != "42" {
		t.Errorf("custom key: %q found=%v", v, found)
	}
	if _, err := ConfigSet(dir, "branch.feat.x.merge", "refs/heads/feat.x", false, false); err != nil {
		t.Fatalf("set subsection: %v", err)
	}
	if v, found, _ := ConfigGet(dir, "branch.feat.x.merge"); !found || v != "refs/heads/feat.x" {
		t.Errorf("subsection key: %q found=%v", v, found)
	}

	if _, err := ConfigSet(dir, "outpost.testkey", "", false, true); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if _, found, _ := ConfigGet(dir, "outpost.testkey"); found {
		t.Errorf("expected key gone after unset")
	}

	// Bad keys are rejected.
	if _, _, err := ConfigGet(dir, "nodots"); err == nil {
		t.Errorf("expected error for key without a section")
	}
}

func TestTagLifecycle(t *testing.T) {
	isolateGlobalConfig(t)
	dir := makeTwoCommitRepo(t)

	if _, err := TagCreate(TagOptions{RepoPath: dir, Name: "v1.0.0"}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	// Tag an older commit explicitly.
	if _, err := TagCreate(TagOptions{RepoPath: dir, Name: "v0.9.0", Commit: "HEAD~1"}); err != nil {
		t.Fatalf("tag at rev: %v", err)
	}
	// Annotated tags need an identity.
	if _, err := TagCreate(TagOptions{RepoPath: dir, Name: "v1.1.0", Message: "release"}); err == nil {
		t.Fatalf("expected identity error for annotated tag")
	}
	if _, err := ConfigSet(dir, "user.name", "T", false, false); err != nil {
		t.Fatalf("config: %v", err)
	}
	if _, err := ConfigSet(dir, "user.email", "t@e", false, false); err != nil {
		t.Fatalf("config: %v", err)
	}
	if _, err := TagCreate(TagOptions{RepoPath: dir, Name: "v1.1.0", Message: "release"}); err != nil {
		t.Fatalf("annotated tag: %v", err)
	}

	tags, err := TagList(dir, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("expected 3 tags, got %v", tags)
	}
	tags, err = TagList(dir, "v1.*")
	if err != nil || len(tags) != 2 {
		t.Fatalf("pattern list: %v %v", tags, err)
	}

	if _, err := TagDelete(dir, "v0.9.0"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	tags, _ = TagList(dir, "")
	for _, tag := range tags {
		if tag == "v0.9.0" {
			t.Errorf("v0.9.0 still listed after delete")
		}
	}
}

func TestResetModes(t *testing.T) {
	dir := makeTwoCommitRepo(t)

	// --hard back one commit: file content reverts, history shrinks.
	if _, err := Reset(ResetOpts{RepoPath: dir, Mode: ResetHard, Commit: "HEAD~1"}); err != nil {
		t.Fatalf("reset --hard: %v", err)
	}
	if got := readFile(t, dir, "a.txt"); got != "line1\n" {
		t.Errorf("hard reset did not revert a.txt: %q", got)
	}
	_, logs, err := Log(LogOptions{RepoPath: dir, Number: 10})
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected 1 commit after hard reset, got %d (%v)", len(logs), err)
	}

	// --soft back: commit again, then soft-reset — changes stay staged.
	commitFiles(t, dir, map[string]string{"a.txt": "line1\nline2\n"}, "redo second")
	if _, err := Reset(ResetOpts{RepoPath: dir, Mode: ResetSoft, Commit: "HEAD~1"}); err != nil {
		t.Fatalf("reset --soft: %v", err)
	}
	_, entries, err := Status(dir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	staged := false
	for _, e := range entries {
		if e.Staged && e.File == "a.txt" {
			staged = true
		}
	}
	if !staged {
		t.Errorf("expected a.txt staged after soft reset, got %v", entries)
	}

	// default (--mixed) unstages.
	if _, err := Reset(ResetOpts{RepoPath: dir}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	_, entries, _ = Status(dir)
	for _, e := range entries {
		if e.Staged {
			t.Errorf("expected nothing staged after mixed reset, got %v", entries)
		}
	}
}

func TestRm(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	commitFiles(t, dir, map[string]string{
		"a.txt":     "a\n",
		"keep.txt":  "keep\n",
		"sub/b.txt": "b\n",
		"sub/c.txt": "c\n",
	}, "seed")

	// Plain rm: gone from disk and index.
	if _, err := Rm(RmOptions{RepoPath: dir, Paths: []string{"a.txt"}}); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("a.txt still on disk")
	}
	paths, err := LsFiles(dir, LsFilesCached)
	if err != nil {
		t.Fatalf("ls-files: %v", err)
	}
	for _, p := range paths {
		if p == "a.txt" {
			t.Errorf("a.txt still tracked")
		}
	}

	// Directory needs -r.
	if _, err := Rm(RmOptions{RepoPath: dir, Paths: []string{"sub"}}); err == nil {
		t.Fatalf("expected -r error for directory")
	}
	if _, err := Rm(RmOptions{RepoPath: dir, Paths: []string{"sub"}, Recursive: true}); err != nil {
		t.Fatalf("rm -r: %v", err)
	}
	paths, _ = LsFiles(dir, LsFilesCached)
	for _, p := range paths {
		if strings.HasPrefix(p, "sub/") {
			t.Errorf("%s still tracked after rm -r", p)
		}
	}

	// --cached: untracked but still on disk.
	if _, err := Rm(RmOptions{RepoPath: dir, Paths: []string{"keep.txt"}, Cached: true}); err != nil {
		t.Fatalf("rm --cached: %v", err)
	}
	if got := readFile(t, dir, "keep.txt"); got != "keep\n" {
		t.Errorf("rm --cached removed the file: %q", got)
	}
	paths, _ = LsFiles(dir, LsFilesCached)
	for _, p := range paths {
		if p == "keep.txt" {
			t.Errorf("keep.txt still tracked after rm --cached")
		}
	}

	// Unknown pathspec errors.
	if _, err := Rm(RmOptions{RepoPath: dir, Paths: []string{"nope.txt"}}); err == nil {
		t.Errorf("expected pathspec error")
	}
}

func TestLsFilesModes(t *testing.T) {
	dir := makeTwoCommitRepo(t)

	paths, err := LsFiles(dir, LsFilesCached)
	if err != nil || len(paths) != 1 || paths[0] != "a.txt" {
		t.Fatalf("cached: %v %v", paths, err)
	}

	// Freshly staged files appear (index, not HEAD tree).
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("s\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Add(AddOptions{RepoPath: dir, Path: "staged.txt"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	paths, _ = LsFiles(dir, LsFilesCached)
	if len(paths) != 2 {
		t.Errorf("expected staged file listed, got %v", paths)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	paths, _ = LsFiles(dir, LsFilesModified)
	if len(paths) != 1 || paths[0] != "a.txt" {
		t.Errorf("modified: %v", paths)
	}
	paths, _ = LsFiles(dir, LsFilesOthers)
	if len(paths) != 1 || paths[0] != "untracked.txt" {
		t.Errorf("others: %v", paths)
	}
}

func TestBlame(t *testing.T) {
	dir := makeTwoCommitRepo(t)

	lines, err := Blame(dir, "a.txt", 0, 0)
	if err != nil {
		t.Fatalf("blame: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0].Author != "T" || lines[0].Text != "line1" {
		t.Errorf("line 1: %+v", lines[0])
	}
	// line2 arrived in the second commit; line1 in the first.
	if lines[0].Hash == lines[1].Hash {
		t.Errorf("expected different commits per line, got %s twice", lines[0].Hash)
	}

	lines, err = Blame(dir, "a.txt", 2, 2)
	if err != nil || len(lines) != 1 || lines[0].LineNo != 2 {
		t.Fatalf("blame -L 2,2: %v %v", lines, err)
	}

	if _, err := Blame(dir, "missing.txt", 0, 0); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	commitFiles(t, dir, map[string]string{
		"a.txt":     "alpha\nTODO fix this\n",
		"sub/b.txt": "todo later\nbeta\n",
	}, "seed")

	matches, err := Grep(GrepOptions{RepoPath: dir, Pattern: "TODO"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(matches) != 1 || matches[0].File != "a.txt" || matches[0].LineNo != 2 {
		t.Fatalf("grep TODO: %+v", matches)
	}

	matches, err = Grep(GrepOptions{RepoPath: dir, Pattern: "todo", IgnoreCase: true})
	if err != nil || len(matches) != 2 {
		t.Fatalf("grep -i: %+v %v", matches, err)
	}

	matches, err = Grep(GrepOptions{RepoPath: dir, Pattern: "todo", IgnoreCase: true, FilesOnly: true})
	if err != nil || len(matches) != 2 {
		t.Fatalf("grep -l: %+v %v", matches, err)
	}

	matches, err = Grep(GrepOptions{RepoPath: dir, Pattern: "todo", IgnoreCase: true, Paths: []string{"sub"}})
	if err != nil || len(matches) != 1 || matches[0].File != "sub/b.txt" {
		t.Fatalf("grep path-limited: %+v %v", matches, err)
	}
}

func TestMergeBaseAndRevList(t *testing.T) {
	dir := makeTwoCommitRepo(t)
	main := currentBranch(t, dir)
	forkPoint, err := RevParse(RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}

	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: "feature", Create: true}); err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	commitFiles(t, dir, map[string]string{"f1.txt": "1\n"}, "feat 1")
	commitFiles(t, dir, map[string]string{"f2.txt": "2\n"}, "feat 2")
	if _, err := Checkout(CheckoutOptions{RepoPath: dir, Branch: main}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	commitFiles(t, dir, map[string]string{"m1.txt": "m\n"}, "main 1")

	bases, err := MergeBase(dir, main, "feature")
	if err != nil {
		t.Fatalf("merge-base: %v", err)
	}
	if len(bases) != 1 || bases[0] != forkPoint.Hash {
		t.Errorf("merge-base = %v, want %s", bases, forkPoint.Hash)
	}

	n, err := RevListCount(dir, main+"..feature")
	if err != nil || n != 2 {
		t.Errorf("rev-list --count %s..feature = %d (%v), want 2", main, n, err)
	}
	n, err = RevListCount(dir, "feature.."+main)
	if err != nil || n != 1 {
		t.Errorf("rev-list --count feature..%s = %d (%v), want 1", main, n, err)
	}
	n, err = RevListCount(dir, "HEAD")
	if err != nil || n != 3 {
		t.Errorf("rev-list --count HEAD = %d (%v), want 3", n, err)
	}
}

func TestDiffCommitsAndShowPatch(t *testing.T) {
	dir := makeTwoCommitRepo(t)

	patch, err := DiffCommits(dir, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(patch, "+line2") {
		t.Errorf("diff missing +line2:\n%s", patch)
	}
	// revB defaults to HEAD.
	patch2, err := DiffCommits(dir, "HEAD~1", "")
	if err != nil || patch2 != patch {
		t.Errorf("default revB mismatch: %v", err)
	}

	_, info, err := Show(ShowOptions{RepoPath: dir, Commit: "HEAD"})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(info.Patch, "+line2") {
		t.Errorf("show patch missing +line2:\n%s", info.Patch)
	}
	// Root commit diffs against the empty tree.
	_, info, err = Show(ShowOptions{RepoPath: dir, Commit: "HEAD~1"})
	if err != nil {
		t.Fatalf("show root: %v", err)
	}
	if !strings.Contains(info.Patch, "+line1") {
		t.Errorf("root-commit patch missing +line1:\n%s", info.Patch)
	}
}

// TestFileTransportIsPureGo guards the no-shell-out invariant: the
// "file" protocol must resolve to this package's in-process transport,
// never go-git's default (which execs git-upload-pack). The whole
// point of outpost git is machines where no system git exists.
func TestFileTransportIsPureGo(t *testing.T) {
	if !InstalledFileTransportIsPureGo() {
		t.Fatalf("file transport is not the in-process server — local-path remotes would shell out to git-upload-pack")
	}
}
