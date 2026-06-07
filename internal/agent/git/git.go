// Package git is outpost's embedded git client. It wraps go-git/v5 to
// provide the typical clone → edit → add → commit → push lifecycle
// plus the common read/inspect verbs (status, log, diff, branch, show,
// remote, fetch, pull). The whole point is to ship a usable git on
// Windows hosts where setting up a system git binary + credentials is
// painful — go-git is pure Go, no cgo, no shell-out, so the same code
// path works on every platform outpost builds for.
//
// Scope intentionally stops at the simple-drop-in line: rebase, stash,
// merge, tag, reset, blame, submodules, worktrees, reflog, bisect are
// not implemented. Users who need those can install system git.
package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Result captures the outcome of a single git operation. Message is
// human-readable; library callers that need machine-readable status
// should branch on the per-operation return type (e.g. []LogEntry).
type Result struct {
	Success bool
	Message string
}

// StatusEntry is one file's status in the working tree.
type StatusEntry struct {
	File   string
	Status string
	Staged bool
}

// AuthConfig is the set of credentials the user can supply. Empty
// fields mean "not provided"; BuildAuthMethod resolves the final
// auth method, including env-var fallbacks.
type AuthConfig struct {
	Username   string
	Password   string
	SSHKey     string
	SSHKeyPass string
}

// BuildAuthMethod resolves AuthConfig to a go-git transport.AuthMethod.
//
// Order of preference:
//  1. Explicit SSHKey path → public-key auth.
//  2. Explicit Username + Password → HTTP basic.
//  3. $GITHUB_TOKEN / $GIT_TOKEN env var → HTTP basic with
//     username="oauth2", password=<token>. This is GitHub's documented
//     token-over-HTTPS idiom and lets a Windows user set one env var
//     instead of wiring a credential helper.
//
// Returns (nil, nil) for the "no auth" case (public clones over HTTPS,
// or SSH against a host trusted via ~/.ssh/known_hosts + ssh-agent).
func BuildAuthMethod(auth AuthConfig) (transport.AuthMethod, error) {
	if auth.SSHKey != "" {
		return gitssh.NewPublicKeysFromFile("git", auth.SSHKey, auth.SSHKeyPass)
	}
	if auth.Username != "" && auth.Password != "" {
		return &githttp.BasicAuth{
			Username: auth.Username,
			Password: auth.Password,
		}, nil
	}
	if tok := firstNonEmptyEnv("GITHUB_TOKEN", "GIT_TOKEN"); tok != "" {
		return &githttp.BasicAuth{
			Username: "oauth2",
			Password: tok,
		}, nil
	}
	return nil, nil
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// CloneOptions configures a Clone call.
type CloneOptions struct {
	URL          string
	Path         string
	Depth        int
	Branch       string // ref name to check out after clone (branch or tag); empty = remote HEAD
	SingleBranch bool   // when true with Branch, fetch only that ref
	Auth         AuthConfig
	Progress     io.Writer
}

// Clone clones URL into Path (or filepath.Base(URL) with .git stripped
// when Path is empty).
func Clone(opts CloneOptions) (*Result, error) {
	if opts.Path == "" {
		opts.Path = strings.TrimSuffix(filepath.Base(opts.URL), ".git")
	}

	cloneOpts := &gogit.CloneOptions{
		URL:      opts.URL,
		Progress: opts.Progress,
	}
	if opts.Depth > 0 {
		cloneOpts.Depth = opts.Depth
	}
	if opts.Branch != "" {
		// go-git resolves branch-or-tag from a ReferenceName; trying
		// branch first then tag matches `git clone --branch` semantics.
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(opts.Branch)
		cloneOpts.SingleBranch = opts.SingleBranch
	}

	auth, err := BuildAuthMethod(opts.Auth)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}
	cloneOpts.Auth = auth

	if _, err := gogit.PlainClone(opts.Path, false, cloneOpts); err != nil {
		// If the branch lookup failed, retry as a tag — go-git won't
		// auto-fall-back like upstream git does.
		if opts.Branch != "" && errors.Is(err, plumbing.ErrReferenceNotFound) {
			cloneOpts.ReferenceName = plumbing.NewTagReferenceName(opts.Branch)
			if _, err2 := gogit.PlainClone(opts.Path, false, cloneOpts); err2 == nil {
				return &Result{Success: true, Message: fmt.Sprintf("Cloned into '%s'", opts.Path)}, nil
			}
		}
		return nil, fmt.Errorf("clone failed: %w", err)
	}

	return &Result{Success: true, Message: fmt.Sprintf("Cloned into '%s'", opts.Path)}, nil
}

// InitOptions configures an Init call.
type InitOptions struct {
	Path string
}

// Init creates an empty repository at Path (or cwd when Path is empty).
func Init(opts InitOptions) (*Result, error) {
	if opts.Path == "" {
		opts.Path = "."
	}
	if _, err := gogit.PlainInit(opts.Path, false); err != nil {
		return nil, fmt.Errorf("init failed: %w", err)
	}
	return &Result{
		Success: true,
		Message: fmt.Sprintf("Initialized empty git repository in %s/.git/", opts.Path),
	}, nil
}

// AddOptions configures an Add call.
type AddOptions struct {
	RepoPath string
	Path     string
	All      bool
}

// Add stages Path (or everything when All is set).
func Add(opts AddOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	if opts.All {
		if _, err := w.Add("."); err != nil {
			return nil, fmt.Errorf("add: %w", err)
		}
		return &Result{Success: true, Message: "Added all changes"}, nil
	}
	if _, err := w.Add(opts.Path); err != nil {
		return nil, fmt.Errorf("add %s: %w", opts.Path, err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Added %s", opts.Path)}, nil
}

// CommitOptions configures a Commit call. AuthorName / AuthorEmail
// override the repo's configured identity; when both are empty the
// underlying go-git fallback to repo/global config applies and will
// surface a "user.name / user.email not configured" error if absent.
type CommitOptions struct {
	RepoPath    string
	Message     string
	Amend       bool
	All         bool
	AuthorName  string
	AuthorEmail string
}

// Commit records a new commit (or amends HEAD when Amend is set).
func Commit(opts CommitOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	commitOpts := &gogit.CommitOptions{All: opts.All, Amend: opts.Amend}
	if opts.AuthorName != "" || opts.AuthorEmail != "" {
		commitOpts.Author = &object.Signature{
			Name:  opts.AuthorName,
			Email: opts.AuthorEmail,
		}
	}
	if opts.Amend {
		if opts.Message == "" {
			headRef, err := r.Head()
			if err != nil {
				return nil, fmt.Errorf("HEAD: %w", err)
			}
			headCommit, err := r.CommitObject(headRef.Hash())
			if err != nil {
				return nil, fmt.Errorf("HEAD commit: %w", err)
			}
			opts.Message = headCommit.Message
		}
	} else if opts.Message == "" {
		return nil, errors.New("commit message required")
	}
	commit, err := w.Commit(opts.Message, commitOpts)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	short := commit.String()
	if len(short) > 7 {
		short = short[:7]
	}
	suffix := ""
	if opts.Amend {
		suffix = " (amended)"
	}
	return &Result{
		Success: true,
		Message: fmt.Sprintf("[%s] %s%s", short, strings.SplitN(opts.Message, "\n", 2)[0], suffix),
	}, nil
}

// Status returns the working-tree state of repoPath. When the tree is
// clean, entries is nil and Result.Message says so; otherwise entries
// has one row per changed file. Callers use entries == nil as the
// clean signal.
func Status(repoPath string) (*Result, []StatusEntry, error) {
	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, nil, fmt.Errorf("worktree: %w", err)
	}
	status, err := w.Status()
	if err != nil {
		return nil, nil, fmt.Errorf("status: %w", err)
	}
	if status.IsClean() {
		return &Result{Success: true, Message: "nothing to commit, working tree clean"}, nil, nil
	}
	var entries []StatusEntry
	for file, s := range status {
		if s.Staging == gogit.Unmodified && s.Worktree != gogit.Unmodified {
			entries = append(entries, StatusEntry{File: file, Status: StatusCode(s.Worktree), Staged: false})
		}
		if s.Staging != gogit.Unmodified {
			entries = append(entries, StatusEntry{File: file, Status: StatusCode(s.Staging), Staged: true})
		}
	}
	return &Result{Success: true}, entries, nil
}

// StatusCode renders a go-git StatusCode as the XY pair upstream git
// uses in `git status --short`.
func StatusCode(status gogit.StatusCode) string {
	switch status {
	case gogit.Untracked:
		return "??"
	case gogit.Modified:
		return " M"
	case gogit.Added:
		return "A "
	case gogit.Deleted:
		return " D"
	case gogit.Renamed:
		return "R "
	case gogit.Copied:
		return "C "
	case gogit.UpdatedButUnmerged:
		return "UU"
	default:
		return "  "
	}
}

// LogOptions configures a Log call.
type LogOptions struct {
	RepoPath string
	Number   int
}

// LogEntry is one row of `git log --oneline`.
type LogEntry struct {
	Hash    string
	Message string
}

// errLogDone is a sentinel used to break out of ForEach once Number
// rows have been collected. Callers ignore it.
var errLogDone = errors.New("git: log done")

// Log returns the first opts.Number commits walking back from HEAD.
func Log(opts LogOptions) (*Result, []LogEntry, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Number <= 0 {
		opts.Number = 10
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("not a git repository: %w", err)
	}
	ref, err := r.Head()
	if err != nil {
		return nil, nil, fmt.Errorf("HEAD: %w", err)
	}
	iter, err := r.Log(&gogit.LogOptions{From: ref.Hash()})
	if err != nil {
		return nil, nil, fmt.Errorf("log: %w", err)
	}
	defer iter.Close()

	var entries []LogEntry
	count := 0
	walkErr := iter.ForEach(func(c *object.Commit) error {
		if count >= opts.Number {
			return errLogDone
		}
		entries = append(entries, LogEntry{
			Hash:    c.Hash.String()[:7],
			Message: strings.SplitN(c.Message, "\n", 2)[0],
		})
		count++
		return nil
	})
	// Shallow clones hit plumbing.ErrObjectNotFound at the depth
	// boundary when the walker tries to load a parent commit that
	// isn't local. Treat that as end-of-history rather than an
	// error; we've already collected everything reachable.
	if walkErr != nil &&
		!errors.Is(walkErr, errLogDone) &&
		!errors.Is(walkErr, plumbing.ErrObjectNotFound) {
		return nil, nil, fmt.Errorf("log walk: %w", walkErr)
	}
	return &Result{Success: true}, entries, nil
}

// PushOptions configures a Push call.
type PushOptions struct {
	RepoPath string
	Remote   string
	Branch   string
	Force    bool
	Auth     AuthConfig
}

// Push pushes the current HEAD branch to opts.Remote/opts.Branch.
func Push(opts PushOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	ref, err := r.Head()
	if err != nil {
		return nil, fmt.Errorf("HEAD: %w", err)
	}
	if opts.Branch == "" {
		opts.Branch = ref.Name().Short()
	}
	auth, err := BuildAuthMethod(opts.Auth)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}
	pushOpts := &gogit.PushOptions{
		RemoteName: opts.Remote,
		RefSpecs:   []config.RefSpec{config.RefSpec(string(ref.Name()) + ":refs/heads/" + opts.Branch)},
		Force:      opts.Force,
		Auth:       auth,
	}
	err = r.Push(pushOpts)
	if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return &Result{Success: true, Message: "Everything up-to-date"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Pushed to %s", opts.Remote)}, nil
}

// PullOptions configures a Pull call.
type PullOptions struct {
	RepoPath string
	Remote   string
	Branch   string
	Auth     AuthConfig
}

// Pull pulls opts.Remote/opts.Branch into the working tree.
func Pull(opts PullOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	auth, err := BuildAuthMethod(opts.Auth)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}
	pullOpts := &gogit.PullOptions{
		RemoteName: opts.Remote,
		Auth:       auth,
	}
	if opts.Branch != "" {
		pullOpts.ReferenceName = plumbing.NewBranchReferenceName(opts.Branch)
	}
	err = w.Pull(pullOpts)
	if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return &Result{Success: true, Message: "Already up to date."}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Pulled from %s", opts.Remote)}, nil
}

// FetchOptions configures a Fetch call.
type FetchOptions struct {
	RepoPath string
	Remote   string
	Auth     AuthConfig
}

// Fetch fetches refs from opts.Remote without updating the working
// tree.
func Fetch(opts FetchOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	auth, err := BuildAuthMethod(opts.Auth)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}
	err = r.Fetch(&gogit.FetchOptions{
		RemoteName: opts.Remote,
		Auth:       auth,
	})
	if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return &Result{Success: true, Message: "Already up to date."}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Fetched from %s", opts.Remote)}, nil
}

// BranchOptions configures a Branch call.
type BranchOptions struct {
	RepoPath string
	Name     string
	Delete   bool
	Force    bool
}

// Branch lists branches when Name is empty, creates a new branch when
// Name is set, or deletes it when Delete is set.
func Branch(opts BranchOptions) (*Result, []string, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("not a git repository: %w", err)
	}
	if opts.Name == "" {
		iter, err := r.Branches()
		if err != nil {
			return nil, nil, fmt.Errorf("branches: %w", err)
		}
		defer iter.Close()
		ref, err := r.Head()
		current := ""
		if err == nil {
			current = ref.Name().Short()
		}
		var branches []string
		_ = iter.ForEach(func(ref *plumbing.Reference) error {
			prefix := "  "
			if ref.Name().Short() == current {
				prefix = "* "
			}
			branches = append(branches, prefix+ref.Name().Short())
			return nil
		})
		return &Result{Success: true}, branches, nil
	}
	if opts.Delete {
		if err := r.Storer.RemoveReference(plumbing.NewBranchReferenceName(opts.Name)); err != nil {
			return nil, nil, fmt.Errorf("delete branch: %w", err)
		}
		return &Result{Success: true, Message: fmt.Sprintf("Deleted branch %s", opts.Name)}, nil, nil
	}
	headRef, err := r.Head()
	if err != nil {
		return nil, nil, fmt.Errorf("HEAD: %w", err)
	}
	newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(opts.Name), headRef.Hash())
	if err := r.Storer.SetReference(newRef); err != nil {
		return nil, nil, fmt.Errorf("create branch: %w", err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Created branch %s", opts.Name)}, nil, nil
}

// CheckoutOptions configures a Checkout call.
type CheckoutOptions struct {
	RepoPath string
	Branch   string
	Create   bool
}

// Checkout switches the working tree to opts.Branch, optionally
// creating it first when Create is set.
func Checkout(opts CheckoutOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	if opts.Create {
		headRef, err := r.Head()
		if err != nil {
			return nil, fmt.Errorf("HEAD: %w", err)
		}
		newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(opts.Branch), headRef.Hash())
		if err := r.Storer.SetReference(newRef); err != nil {
			return nil, fmt.Errorf("create branch: %w", err)
		}
	}
	if err := w.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(opts.Branch),
	}); err != nil {
		return nil, fmt.Errorf("checkout: %w", err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Switched to branch '%s'", opts.Branch)}, nil
}

// RemoteEntry is one configured remote.
type RemoteEntry struct {
	Name string
	URLs []string
}

// Remotes lists the configured remotes for repoPath.
func Remotes(repoPath string) (*Result, []RemoteEntry, error) {
	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("not a git repository: %w", err)
	}
	remotes, err := r.Remotes()
	if err != nil {
		return nil, nil, fmt.Errorf("list remotes: %w", err)
	}
	var entries []RemoteEntry
	for _, remote := range remotes {
		entries = append(entries, RemoteEntry{
			Name: remote.Config().Name,
			URLs: remote.Config().URLs,
		})
	}
	return &Result{Success: true}, entries, nil
}

// ShowOptions configures a Show call.
type ShowOptions struct {
	RepoPath string
	Commit   string
}

// ShowResult is the parsed view of one commit.
type ShowResult struct {
	Hash    string
	Author  string
	Email   string
	Date    string
	Message string
}

// Show resolves opts.Commit (defaulting to HEAD) and returns its
// metadata.
func Show(opts ShowOptions) (*Result, *ShowResult, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Commit == "" {
		opts.Commit = "HEAD"
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("not a git repository: %w", err)
	}
	var hash plumbing.Hash
	if opts.Commit == "HEAD" {
		ref, err := r.Head()
		if err != nil {
			return nil, nil, fmt.Errorf("HEAD: %w", err)
		}
		hash = ref.Hash()
	} else {
		// Resolve a possibly-short hash or a ref name via go-git's
		// revision resolver (handles "v1.2", "main", "abcdef1", etc.).
		resolved, err := r.ResolveRevision(plumbing.Revision(opts.Commit))
		if err != nil {
			return nil, nil, fmt.Errorf("resolve %q: %w", opts.Commit, err)
		}
		hash = *resolved
	}
	commit, err := r.CommitObject(hash)
	if err != nil {
		return nil, nil, fmt.Errorf("commit %s: %w", hash.String(), err)
	}
	return &Result{Success: true}, &ShowResult{
		Hash:    commit.Hash.String(),
		Author:  commit.Author.Name,
		Email:   commit.Author.Email,
		Date:    commit.Author.When.Format("Mon Jan 2 15:04:05 2006 -0700"),
		Message: commit.Message,
	}, nil
}
