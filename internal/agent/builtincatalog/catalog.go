// Package builtincatalog resolves operator-facing built-in names to manifests
// in the open-source dhnt/appstore checkout. It deliberately knows nothing
// about cloudbox's private embedded manifests.
package builtincatalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var validName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// Entry is one resolved OSS appstore built-in.
type Entry struct {
	Name     string
	Catalog  string
	Manifest string
}

// Resolve maps name to <catalog>/builtin/<name>/install.yaml. catalog may be
// an umbrella sibling appstore checkout or a fetched/versioned appstore tree.
// Empty catalog discovers only deterministic development-checkout locations.
func Resolve(catalog, name string) (Entry, error) {
	name = strings.TrimSpace(name)
	if !validName.MatchString(name) {
		return Entry{}, fmt.Errorf("invalid built-in name %q", name)
	}
	root, err := ResolveCatalog(catalog)
	if err != nil {
		return Entry{}, err
	}
	manifest := filepath.Join(root, "builtin", name, "install.yaml")
	manifest, err = filepath.EvalSymlinks(manifest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, fmt.Errorf("built-in %q is unavailable in catalog %q", name, root)
		}
		return Entry{}, fmt.Errorf("resolve built-in %q: %w", name, err)
	}
	if !within(root, manifest) {
		return Entry{}, fmt.Errorf("built-in %q escapes catalog %q", name, root)
	}
	info, err := os.Stat(manifest)
	if err != nil {
		return Entry{}, fmt.Errorf("inspect built-in %q: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("built-in %q manifest is not a regular file", name)
	}
	return Entry{Name: name, Catalog: root, Manifest: manifest}, nil
}

// ResolveCatalog canonicalizes an explicit root, or discovers an appstore
// sibling in an umbrella source checkout. There is intentionally no network
// fallback: unavailable assets fail closed.
func ResolveCatalog(catalog string) (string, error) {
	if strings.TrimSpace(catalog) != "" {
		return canonicalCatalog(strings.TrimSpace(catalog))
	}
	cwd, cwdErr := os.Getwd()
	var candidates []string
	if cwdErr == nil {
		candidates = append(candidates, filepath.Join(cwd, "appstore"))
		if filepath.Base(cwd) == "outpost" {
			candidates = append(candidates, filepath.Join(filepath.Dir(cwd), "appstore"))
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "appstore"), filepath.Join(filepath.Dir(dir), "appstore"))
	}
	for _, candidate := range candidates {
		root, err := canonicalCatalog(candidate)
		if err == nil {
			return root, nil
		}
	}
	return "", fmt.Errorf("OSS built-in catalog unavailable; pass an appstore checkout with --catalog or persist cluster.bundle_catalog")
}

// List returns built-ins which have a regular install.yaml within the catalog.
func List(catalog string) (string, []string, error) {
	root, err := ResolveCatalog(catalog)
	if err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "builtin"))
	if err != nil {
		return "", nil, fmt.Errorf("read built-in catalog %q: %w", root, err)
	}
	var names []string
	for _, dir := range entries {
		if !dir.IsDir() || !validName.MatchString(dir.Name()) {
			continue
		}
		if _, err := Resolve(root, dir.Name()); err == nil {
			names = append(names, dir.Name())
		}
	}
	sort.Strings(names)
	return root, names, nil
}

func canonicalCatalog(path string) (string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("catalog %q: %w", path, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("catalog %q: %w", path, err)
	}
	info, err := os.Stat(filepath.Join(root, "builtin"))
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("catalog %q has no builtin directory", root)
	}
	return root, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
