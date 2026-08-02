// Package appcatalog resolves operator-facing public app ids to a Helm
// chart manifest pair — apps/<id>/app.yaml plus an optional
// apps/<id>/values.yaml — in an open-source dhnt/appstore checkout.
//
// This is the "apps" half of the peer-DKS appstore parity surface;
// internal/agent/builtincatalog is the "builtins" half (raw
// builtin/<name>/install.yaml manifests). The two trees live side by
// side in the same catalog root but resolve independently: an apps/<id>
// entry names a Helm chart (repo/name/version + values), never a raw
// manifest, and is rendered into a k3s helm.cattle.io/v1 HelmChart
// object (see render.go) rather than applied byte-for-byte.
//
// Confinement mirrors builtincatalog's approach deliberately: a
// symlink-resolved, traversal-checked path under the catalog root, an
// operator-name regex, and a required-subdirectory probe for catalog
// discovery. It knows nothing about cloudbox's private manifests.
package appcatalog

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

// Entry is one resolved OSS appstore app.
type Entry struct {
	ID      string
	Catalog string
	// AppFile is <catalog>/apps/<id>/app.yaml — always present.
	AppFile string
	// ValuesFile is <catalog>/apps/<id>/values.yaml — "" when the app
	// ships no values override (a chart with sane defaults needs none).
	ValuesFile string
}

// Resolve maps id to <catalog>/apps/<id>/app.yaml (+ an optional sibling
// values.yaml). catalog may be an umbrella sibling appstore checkout or a
// fetched/versioned appstore tree. Empty catalog discovers only
// deterministic development-checkout locations.
func Resolve(catalog, id string) (Entry, error) {
	id = strings.TrimSpace(id)
	if !validName.MatchString(id) {
		return Entry{}, fmt.Errorf("invalid app id %q", id)
	}
	root, err := ResolveCatalog(catalog)
	if err != nil {
		return Entry{}, err
	}
	appFile, err := resolveAsset(root, id, "app.yaml", true)
	if err != nil {
		return Entry{}, err
	}
	valuesFile, err := resolveAsset(root, id, "values.yaml", false)
	if err != nil {
		return Entry{}, err
	}
	return Entry{ID: id, Catalog: root, AppFile: appFile, ValuesFile: valuesFile}, nil
}

// resolveAsset resolves <root>/apps/<id>/<filename>, refusing anything
// that escapes root via symlink or traversal. When required is false, a
// missing file is reported as ("", nil) rather than an error.
func resolveAsset(root, id, filename string, required bool) (string, error) {
	path := filepath.Join(root, "apps", id, filename)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if required {
				return "", fmt.Errorf("app %q is unavailable in catalog %q (missing %s)", id, root, filename)
			}
			return "", nil
		}
		return "", fmt.Errorf("resolve app %q %s: %w", id, filename, err)
	}
	if !within(root, resolved) {
		return "", fmt.Errorf("app %q %s escapes catalog %q", id, filename, root)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect app %q %s: %w", id, filename, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("app %q %s is not a regular file", id, filename)
	}
	return resolved, nil
}

// ResolveCatalog canonicalizes an explicit root, or discovers an appstore
// sibling in an umbrella source checkout. There is intentionally no
// network fallback: unavailable assets fail closed.
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
	return "", fmt.Errorf("OSS appstore catalog unavailable; pass an appstore checkout with --catalog or persist cluster.bundle_catalog")
}

// List returns apps which have a regular app.yaml within the catalog.
func List(catalog string) (string, []string, error) {
	root, err := ResolveCatalog(catalog)
	if err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "apps"))
	if err != nil {
		return "", nil, fmt.Errorf("read app catalog %q: %w", root, err)
	}
	var ids []string
	for _, dir := range entries {
		if !dir.IsDir() || !validName.MatchString(dir.Name()) {
			continue
		}
		if _, err := Resolve(root, dir.Name()); err == nil {
			ids = append(ids, dir.Name())
		}
	}
	sort.Strings(ids)
	return root, ids, nil
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
	info, err := os.Stat(filepath.Join(root, "apps"))
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("catalog %q has no apps directory", root)
	}
	return root, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
