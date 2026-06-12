package git

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TagOptions configures a TagCreate call.
type TagOptions struct {
	RepoPath string
	Name     string
	Commit   string // revision to tag; empty = HEAD
	Message  string // non-empty (or Annotate) creates an annotated tag
	Annotate bool
}

// TagList returns tag names, optionally filtered by a filepath.Match
// pattern (`git tag -l 'v1.*'`), sorted.
func TagList(repoPath, pattern string) ([]string, error) {
	if repoPath == "" {
		repoPath = "."
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	iter, err := r.Tags()
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	defer iter.Close()
	var tags []string
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if pattern != "" {
			if ok, _ := filepath.Match(pattern, name); !ok {
				return nil
			}
		}
		tags = append(tags, name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	sort.Strings(tags)
	return tags, nil
}

// TagCreate creates a lightweight tag, or an annotated one when
// Annotate is set or a Message is given. Annotated tags need a
// configured identity (same rule as commit).
func TagCreate(opts TagOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Name == "" {
		return nil, errors.New("tag: name required")
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	target := opts.Commit
	if target == "" {
		target = "HEAD"
	}
	commit, err := resolveCommit(r, target)
	if err != nil {
		return nil, err
	}
	var createOpts *gogit.CreateTagOptions
	if opts.Annotate || opts.Message != "" {
		msg := opts.Message
		if msg == "" {
			msg = opts.Name
		}
		sig, err := commitSignature(r)
		if err != nil {
			return nil, err
		}
		createOpts = &gogit.CreateTagOptions{Message: msg, Tagger: sig}
	}
	if _, err := r.CreateTag(opts.Name, commit.Hash, createOpts); err != nil {
		return nil, fmt.Errorf("tag %s: %w", opts.Name, err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Created tag %s at %s", opts.Name, shortHash(commit.Hash))}, nil
}

// TagDelete removes a tag.
func TagDelete(repoPath, name string) (*Result, error) {
	if repoPath == "" {
		repoPath = "."
	}
	if name == "" {
		return nil, errors.New("tag: name required")
	}
	r, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	if err := r.DeleteTag(name); err != nil {
		return nil, fmt.Errorf("delete tag %s: %w", name, err)
	}
	return &Result{Success: true, Message: fmt.Sprintf("Deleted tag %s", name)}, nil
}
