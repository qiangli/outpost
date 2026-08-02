package bundleapply

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Venue guard — the Go-API enforcement that a peer bundle apply never
// lands on the cloudbox kubeconfig.
//
// The check canonicalizes BOTH sides before comparing: the target path is
// tilde-expanded, made absolute, and symlink-resolved (EvalSymlinks), so
// reaching ~/.kube/outpost.yaml via a symlink, a relative path, or a
// `..` traversal is still recognized as the cloudbox plane and refused.
// A target that cannot be canonicalized (missing file, dangling symlink,
// unresolvable component) FAILS — it is not "probably fine".
//
// The enforcement lives HERE, not only in the operator script: every
// production ResourceClient constructor (NewDynamicClient) and the
// admincore surface call ResolveVenue, so a caller using the package
// directly cannot bypass the guard.

// CanonicalizePath expands a leading ~, makes the path absolute, and
// resolves every symlink. The path must exist — EvalSymlinks on a missing
// file is an error, which is exactly the evidence invariant: a venue we
// cannot positively resolve is a failure, never a pass.
func CanonicalizePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("bundleapply: empty path")
	}
	expanded, err := ExpandUser(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("bundleapply: absolutize %q: %w", path, err)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("bundleapply: canonicalize %q: %w", path, err)
	}
	return canon, nil
}

// canonicalizeBestEffort canonicalizes a REFERENCE path (the cloudbox
// kubeconfig we compare against) without requiring it to exist: when the
// file itself cannot be symlink-resolved, the deepest existing ancestor
// is resolved and the remainder re-joined. Best-effort is safe on this
// side of the comparison — a reference that resolves less precisely can
// only make the guard MISS an equality, and the target side is still
// hard-canonicalized, so a symlinked route to an existing cloudbox file
// is always caught.
func canonicalizeBestEffort(path string) string {
	expanded, err := ExpandUser(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return filepath.Clean(expanded)
	}
	if canon, err := filepath.EvalSymlinks(abs); err == nil {
		return canon
	}
	dir, rest := abs, ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
		if canon, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(canon, rest)
		}
	}
	return abs
}

// ResolveVenue canonicalizes a kubeconfig path and enforces the venue
// guard: the result must not be the cloudbox kubeconfig, however it was
// reached. Returns the canonical path to use.
func ResolveVenue(path string) (string, error) {
	return resolveVenueAgainst(path, CloudboxKubeconfig)
}

// resolveVenueAgainst is ResolveVenue with an injectable cloudbox
// reference path, so tests can exercise the symlink/traversal cases in a
// temp dir without touching the real ~/.kube.
func resolveVenueAgainst(path, cloudboxPath string) (string, error) {
	canon, err := CanonicalizePath(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrVenueUnresolvable, err)
	}
	if canon == canonicalizeBestEffort(cloudboxPath) {
		return "", fmt.Errorf("%w (kubeconfig %q resolves to %q)", ErrCloudboxVenue, path, canon)
	}
	return canon, nil
}
