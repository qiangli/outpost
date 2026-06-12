package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
)

// ResetMode mirrors git's reset flavors.
type ResetMode string

const (
	ResetSoft  ResetMode = "soft"  // move HEAD only
	ResetMixed ResetMode = "mixed" // move HEAD + reset index (default)
	ResetHard  ResetMode = "hard"  // move HEAD + reset index + working tree
)

// ResetOpts configures a Reset call.
type ResetOpts struct {
	RepoPath string
	Mode     ResetMode
	Commit   string // revision to reset to; empty = HEAD
}

// Reset implements `git reset [--soft|--mixed|--hard] [commit]`.
//
// Caveat vs. real git, inherited from go-git: --hard also deletes
// untracked files. Reset is the one verb where that's at least
// adjacent to the operator's intent, but it's still a difference
// worth knowing — the CLI help calls it out.
func Reset(opts ResetOpts) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Mode == "" {
		opts.Mode = ResetMixed
	}
	var mode gogit.ResetMode
	switch opts.Mode {
	case ResetSoft:
		mode = gogit.SoftReset
	case ResetMixed:
		mode = gogit.MixedReset
	case ResetHard:
		mode = gogit.HardReset
	default:
		return nil, fmt.Errorf("reset: unknown mode %q", opts.Mode)
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	target := opts.Commit
	if target == "" {
		target = "HEAD"
	}
	commit, err := resolveCommit(r, target)
	if err != nil {
		return nil, err
	}
	if err := w.Reset(&gogit.ResetOptions{Mode: mode, Commit: commit.Hash}); err != nil {
		return nil, fmt.Errorf("reset: %w", err)
	}
	first := strings.SplitN(commit.Message, "\n", 2)[0]
	return &Result{Success: true, Message: fmt.Sprintf("HEAD is now at %s %s", shortHash(commit.Hash), first)}, nil
}

// RmOptions configures an Rm call.
type RmOptions struct {
	RepoPath  string
	Paths     []string
	Cached    bool // remove from the index only, keep the file on disk
	Recursive bool // allow directory arguments
}

// Rm implements `git rm [--cached] [-r] <path>...` — removes paths
// from the index and (unless Cached) the working tree.
func Rm(opts RmOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if len(opts.Paths) == 0 {
		return nil, errors.New("rm: at least one path required")
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	idx, err := r.Storer.Index()
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	root := w.Filesystem.Root()

	// Expand each argument to the tracked files it covers, mirroring
	// git's pathspec rules: a file matches itself; a directory needs -r
	// and matches every tracked file under it.
	var files []string
	for _, p := range opts.Paths {
		p = filepath.ToSlash(filepath.Clean(p))
		var matched []string
		isDir := false
		for _, e := range idx.Entries {
			if e.Name == p {
				matched = append(matched, e.Name)
			} else if strings.HasPrefix(e.Name, p+"/") {
				isDir = true
				matched = append(matched, e.Name)
			}
		}
		if len(matched) == 0 {
			return nil, fmt.Errorf("rm: pathspec %q did not match any tracked files", p)
		}
		if isDir && !opts.Recursive {
			return nil, fmt.Errorf("rm: not removing %q recursively without -r", p)
		}
		files = append(files, matched...)
	}

	removed := 0
	for _, name := range files {
		var keep []byte
		var keepMode os.FileMode
		full := filepath.Join(root, filepath.FromSlash(name))
		if opts.Cached {
			// Worktree.Remove deletes both the index entry and the file;
			// snapshot the on-disk bytes first and restore them after so
			// --cached leaves the working copy untouched.
			fi, err := os.Lstat(full)
			if err == nil && fi.Mode().IsRegular() {
				if keep, err = os.ReadFile(full); err != nil {
					return nil, fmt.Errorf("rm --cached: read %s: %w", name, err)
				}
				keepMode = fi.Mode()
			}
		}
		if _, err := w.Remove(name); err != nil {
			return nil, fmt.Errorf("rm %s: %w", name, err)
		}
		if keep != nil {
			if err := os.WriteFile(full, keep, keepMode.Perm()); err != nil {
				return nil, fmt.Errorf("rm --cached: restore %s: %w", name, err)
			}
		}
		removed++
	}
	suffix := ""
	if opts.Cached {
		suffix = " (index only)"
	}
	return &Result{Success: true, Message: fmt.Sprintf("Removed %d file(s)%s", removed, suffix)}, nil
}
