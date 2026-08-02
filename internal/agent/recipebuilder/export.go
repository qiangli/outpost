package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// This file is the exported surface the PEER half of recipe distribution
// (internal/agent/peerimage) builds on. The cloudbox-driven Builder above and
// the peer-driven Service there must agree byte-for-byte on what a recipe IS
// and on how it is materialized, so both go through these functions rather
// than reimplementing parsing or the build sequence.

// ParseRecipe decodes and validates a flat ImageRecipe YAML document. A recipe
// that does not validate is an error, never a partially-populated Recipe — a
// half-parsed recipe is exactly the kind of thing that builds the wrong image.
func ParseRecipe(body string) (Recipe, error) {
	r := parseRecipe(body)
	if err := r.validate(); err != nil {
		return Recipe{}, err
	}
	return r, nil
}

// Ref is the image reference this recipe builds to ("<local_ref>:<tag>").
func (r Recipe) Ref() string { return r.ref() }

// Canonical is the recipe's deterministic identity form: sorted "key=value"
// lines over the fields that decide WHAT gets built. Formatting, comment and
// key-order differences in the authored YAML do not change it.
//
// The inline archive itself is represented by context_sha256 rather than by its
// bytes. That digest is verified byte-for-byte before extraction (see
// decodeInlineArchive), so it is a faithful stand-in for the source and keeps
// the canonical form small enough to log and compare.
func (r Recipe) Canonical() []byte {
	fields := map[string]string{
		"name":           r.Name,
		"tag":            r.Tag,
		"local_ref":      r.LocalRef,
		"context_type":   r.ContextType,
		"context_repo":   r.ContextRepo,
		"context_ref":    r.ContextRef,
		"context_subdir": r.ContextSubdir,
		"context_path":   r.ContextPath,
		"dockerfile":     r.Dockerfile,
		"context_sha256": r.ContextSha256,
		"base_images":    strings.Join(r.BaseImages, " "),
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, fields[k])
	}
	return []byte(b.String())
}

// Digest is "sha256:<hex>" over Canonical() — the recipe's CROSS-NODE identity.
// Two nodes that built from the same recipe agree on this value even though
// their resulting image content digests differ (container builds are not
// bit-reproducible).
func (r Recipe) Digest() string {
	sum := sha256.Sum256(r.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Materialize resolves a recipe's build context, builds it NATIVELY for
// platform, and loads it into runtimeContainer's containerd. It is the exact
// sequence the polling Builder runs, factored out so the peer path cannot
// drift from it.
//
// It does not verify the result; callers confirm residency by content digest.
func Materialize(ctx context.Context, r Runner, workDir, platform string, rec Recipe, runtimeContainer string) error {
	b := &Builder{
		cfg:      Config{RuntimeContainer: runtimeContainer},
		runner:   r,
		platform: platform,
		workDir:  workDir,
		built:    map[string]string{},
	}
	return b.materialize(ctx, rec)
}

// NewBashyRunner returns a Runner that shells out to `bashy podman`/`bashy
// git`, resolving the bashy executable through path on EVERY call. Construction
// therefore never blocks on provisioning a missing userland: the daemon wires
// path to its self-healing resolver (cmd/outpost's bashyResolver.Path), whose
// cached fast path makes repeat calls cheap, and a host that lacks bashy today
// recovers the moment one appears rather than needing a daemon restart.
func NewBashyRunner(path func(ctx context.Context) (string, error)) Runner {
	return &bashyRunner{path: path}
}

// bashyRunner adapts the lazily-resolved bashy executable to the Runner
// interface, one fresh execRunner per call.
type bashyRunner struct {
	path func(ctx context.Context) (string, error)
}

func (r *bashyRunner) resolve(ctx context.Context) (execRunner, error) {
	if r.path == nil {
		return execRunner{}, fmt.Errorf("no bashy path resolver is configured")
	}
	bin, err := r.path(ctx)
	if err != nil {
		return execRunner{}, err
	}
	if strings.TrimSpace(bin) == "" {
		return execRunner{}, fmt.Errorf("bashy path resolver returned an empty path")
	}
	return execRunner{bashyBin: bin}, nil
}

func (r *bashyRunner) Clone(ctx context.Context, repo, ref, dest string) error {
	e, err := r.resolve(ctx)
	if err != nil {
		return err
	}
	return e.Clone(ctx, repo, ref, dest)
}

func (r *bashyRunner) Build(ctx context.Context, platform, dockerfile, ref, contextDir string) error {
	e, err := r.resolve(ctx)
	if err != nil {
		return err
	}
	return e.Build(ctx, platform, dockerfile, ref, contextDir)
}

func (r *bashyRunner) Load(ctx context.Context, ref, runtimeContainer string) error {
	e, err := r.resolve(ctx)
	if err != nil {
		return err
	}
	return e.Load(ctx, ref, runtimeContainer)
}

func (r *bashyRunner) ImagePresent(ctx context.Context, ref, runtimeContainer string) (bool, error) {
	e, err := r.resolve(ctx)
	if err != nil {
		return false, err
	}
	return e.ImagePresent(ctx, ref, runtimeContainer)
}
