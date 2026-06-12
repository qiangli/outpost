package git

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// resolveCommit resolves a revision string (branch, tag, short/full
// SHA, HEAD~N, …) to its commit object.
func resolveCommit(r *gogit.Repository, rev string) (*object.Commit, error) {
	hash, err := r.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", rev, err)
	}
	c, err := r.CommitObject(*hash)
	if err != nil {
		return nil, fmt.Errorf("commit %s: %w", hash, err)
	}
	return c, nil
}

// MergeBase returns the best common ancestor(s) of two revisions, one
// full SHA per entry — the same output shape as `git merge-base`.
func MergeBase(repoPath, revA, revB string) ([]string, error) {
	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	ca, err := resolveCommit(r, revA)
	if err != nil {
		return nil, err
	}
	cb, err := resolveCommit(r, revB)
	if err != nil {
		return nil, err
	}
	bases, err := ca.MergeBase(cb)
	if err != nil {
		return nil, fmt.Errorf("merge-base: %w", err)
	}
	if len(bases) == 0 {
		return nil, fmt.Errorf("merge-base: %q and %q have no common ancestor", revA, revB)
	}
	var out []string
	for _, b := range bases {
		out = append(out, b.Hash.String())
	}
	return out, nil
}

// countRange counts commits reachable from "to" but not from "from" —
// the `git rev-list --count from..to` number. Shallow-clone walks that
// hit the depth boundary count what's reachable locally.
func countRange(r *gogit.Repository, from, to plumbing.Hash) (int, error) {
	exclude := map[plumbing.Hash]bool{}
	if from != plumbing.ZeroHash {
		iter, err := r.Log(&gogit.LogOptions{From: from})
		if err != nil {
			return 0, fmt.Errorf("rev-list: %w", err)
		}
		err = iter.ForEach(func(c *object.Commit) error {
			exclude[c.Hash] = true
			return nil
		})
		iter.Close()
		if err != nil && !errors.Is(err, plumbing.ErrObjectNotFound) {
			return 0, fmt.Errorf("rev-list walk: %w", err)
		}
	}
	iter, err := r.Log(&gogit.LogOptions{From: to})
	if err != nil {
		return 0, fmt.Errorf("rev-list: %w", err)
	}
	defer iter.Close()
	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if !exclude[c.Hash] {
			count++
		}
		return nil
	})
	if err != nil && !errors.Is(err, plumbing.ErrObjectNotFound) {
		return 0, fmt.Errorf("rev-list walk: %w", err)
	}
	return count, nil
}

// RevListCount implements `git rev-list --count <spec>` where spec is
// "A..B", "A...B" (treated as A..B), or a single revision (full count
// of its history).
func RevListCount(repoPath, spec string) (int, error) {
	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return 0, fmt.Errorf("not a git repository: %w", err)
	}
	from, to := "", spec
	if parts := strings.SplitN(spec, "...", 2); len(parts) == 2 {
		from, to = parts[0], parts[1]
	} else if parts := strings.SplitN(spec, "..", 2); len(parts) == 2 {
		from, to = parts[0], parts[1]
	}
	fromHash := plumbing.ZeroHash
	if from != "" {
		c, err := resolveCommit(r, from)
		if err != nil {
			return 0, err
		}
		fromHash = c.Hash
	}
	cTo, err := resolveCommit(r, to)
	if err != nil {
		return 0, err
	}
	return countRange(r, fromHash, cTo.Hash)
}

// LsFilesMode selects what LsFiles lists.
type LsFilesMode string

const (
	LsFilesCached   LsFilesMode = ""         // tracked files, from the index (includes staged adds)
	LsFilesModified LsFilesMode = "modified" // tracked files with worktree modifications
	LsFilesOthers   LsFilesMode = "others"   // untracked files
)

// LsFiles lists paths per mode, sorted. The default (cached) reads the
// index rather than the HEAD tree, so freshly-staged files appear —
// matching real `git ls-files`.
func LsFiles(repoPath string, mode LsFilesMode) ([]string, error) {
	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	var paths []string
	switch mode {
	case LsFilesCached:
		idx, err := r.Storer.Index()
		if err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		for _, e := range idx.Entries {
			paths = append(paths, e.Name)
		}
	case LsFilesModified, LsFilesOthers:
		w, err := r.Worktree()
		if err != nil {
			return nil, fmt.Errorf("worktree: %w", err)
		}
		st, err := w.Status()
		if err != nil {
			return nil, fmt.Errorf("status: %w", err)
		}
		for path, fs := range st {
			switch mode {
			case LsFilesModified:
				if fs.Worktree == gogit.Modified || fs.Worktree == gogit.Deleted || fs.Staging == gogit.Modified {
					paths = append(paths, path)
				}
			case LsFilesOthers:
				if fs.Worktree == gogit.Untracked {
					paths = append(paths, path)
				}
			}
		}
	default:
		return nil, fmt.Errorf("ls-files: unknown mode %q", mode)
	}
	sort.Strings(paths)
	return paths, nil
}

// BlameLine is one annotated line of a file.
type BlameLine struct {
	Hash   string
	Author string
	Date   string
	LineNo int
	Text   string
}

// Blame annotates file (path relative to the repo root) at HEAD.
// start/end bound the reported lines (1-based, inclusive); 0 means
// unbounded — the `git blame -L start,end` subset.
func Blame(repoPath, file string, start, end int) ([]BlameLine, error) {
	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	head, err := r.Head()
	if err != nil {
		return nil, fmt.Errorf("HEAD: %w", err)
	}
	commit, err := r.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("HEAD commit: %w", err)
	}
	br, err := gogit.Blame(commit, file)
	if err != nil {
		return nil, fmt.Errorf("blame %s: %w", file, err)
	}
	var lines []BlameLine
	for i, l := range br.Lines {
		n := i + 1
		if start > 0 && n < start {
			continue
		}
		if end > 0 && n > end {
			break
		}
		// go-git's Line.Author is the email; AuthorName is the name.
		author := l.AuthorName
		if author == "" {
			author = l.Author
		}
		lines = append(lines, BlameLine{
			Hash:   l.Hash.String()[:8],
			Author: author,
			Date:   l.Date.Format("2006-01-02"),
			LineNo: n,
			Text:   l.Text,
		})
	}
	return lines, nil
}

// GrepOptions configures a Grep call.
type GrepOptions struct {
	RepoPath   string
	Pattern    string // Go regexp, matched per line
	IgnoreCase bool
	FilesOnly  bool     // report each matching file once (git grep -l)
	Paths      []string // optional path prefixes to restrict the search
}

// GrepMatch is one matching line.
type GrepMatch struct {
	File    string
	LineNo  int
	Content string
}

// Grep searches the HEAD tree for a regexp, like `git grep` over the
// committed content.
func Grep(opts GrepOptions) ([]GrepMatch, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Pattern == "" {
		return nil, errors.New("grep: pattern required")
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	pat := opts.Pattern
	if opts.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("grep: bad pattern: %w", err)
	}
	gopts := &gogit.GrepOptions{Patterns: []*regexp.Regexp{re}}
	for _, p := range opts.Paths {
		pre, err := regexp.Compile("^" + regexp.QuoteMeta(strings.TrimSuffix(p, "/")))
		if err != nil {
			return nil, fmt.Errorf("grep: bad path %q: %w", p, err)
		}
		gopts.PathSpecs = append(gopts.PathSpecs, pre)
	}
	results, err := r.Grep(gopts)
	if err != nil {
		return nil, fmt.Errorf("grep: %w", err)
	}
	var matches []GrepMatch
	seenFile := map[string]bool{}
	for _, res := range results {
		if opts.FilesOnly {
			if seenFile[res.FileName] {
				continue
			}
			seenFile[res.FileName] = true
			matches = append(matches, GrepMatch{File: res.FileName})
			continue
		}
		matches = append(matches, GrepMatch{File: res.FileName, LineNo: res.LineNumber, Content: res.Content})
	}
	return matches, nil
}

// DiffCommits returns the unified patch between two revisions — the
// `git diff A B` text. revB defaults to HEAD when empty.
func DiffCommits(repoPath, revA, revB string) (string, error) {
	if repoPath == "" {
		repoPath = "."
	}
	if revB == "" {
		revB = "HEAD"
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	ca, err := resolveCommit(r, revA)
	if err != nil {
		return "", err
	}
	cb, err := resolveCommit(r, revB)
	if err != nil {
		return "", err
	}
	patch, err := ca.Patch(cb)
	if err != nil {
		return "", fmt.Errorf("diff: %w", err)
	}
	return patch.String(), nil
}

// commitPatch renders the patch a commit introduced relative to its
// first parent (root commits diff against the empty tree). Used by
// Show for git-like `git show` output.
func commitPatch(commit *object.Commit) (string, error) {
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("tree: %w", err)
	}
	parentTree := &object.Tree{}
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err != nil {
			return "", fmt.Errorf("parent: %w", err)
		}
		if parentTree, err = parent.Tree(); err != nil {
			return "", fmt.Errorf("parent tree: %w", err)
		}
	}
	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		return "", fmt.Errorf("diff: %w", err)
	}
	patch, err := changes.Patch()
	if err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	return patch.String(), nil
}
