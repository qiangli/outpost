package appcatalog

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion / Kind are the ONLY apps/<id>/app.yaml envelope this package
// understands — they mirror the real dhnt/appstore catalog format exactly
// (see ../appstore/apps/<id>/app.yaml). Any other value — including a
// missing one — fails closed; there is no best-effort parse of an unknown
// shape.
const (
	APIVersion = "appstore.dhnt.io/v1"
	Kind       = "AppEntry"
)

// safeVersion constrains chart.version to an inert, injection-free token —
// a leading alphanumeric followed by the semver-ish set (dots, dashes,
// pluses, underscores). It rejects whitespace, path/URL/shell
// metacharacters, and the empty string. The value is only ever carried as
// structured data into a HelmChart CR (never a command line), but a strict
// charset keeps a malformed catalog entry from ever reaching the cluster.
var safeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// ChartRef names the Helm chart an app.yaml installs (spec.chart).
type ChartRef struct {
	// Repo is the Helm chart repository URL. Must be https:// or oci://
	// — never a bare string that could be shell-interpreted.
	Repo string `yaml:"repo"`
	// Name is the chart name within Repo.
	Name string `yaml:"name"`
	// Version pins an exact chart version — no floating "latest".
	Version string `yaml:"version"`
}

// Maintainer mirrors one metadata.maintainers[] entry. Advisory only —
// never validated or acted on.
type Maintainer struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// RBAC mirrors spec.rbac. clusterScoped tells cloudbox whether the chart
// needs cluster-scoped resources; on this peer-hosted path it is surfaced
// but the install always targets the caller's own namespace.
type RBAC struct {
	ClusterScoped bool `yaml:"clusterScoped"`
}

// Metadata mirrors the app.yaml metadata block. Only id is load-bearing
// (it must match the catalog directory name); everything else is
// operator-facing catalog copy carried through to Show.
type Metadata struct {
	ID          string       `yaml:"id"`
	Name        string       `yaml:"name"`
	Version     string       `yaml:"version"`
	Categories  []string     `yaml:"categories"`
	Tags        []string     `yaml:"tags"`
	Maintainers []Maintainer `yaml:"maintainers"`
	Description string       `yaml:"description"`
	Homepage    string       `yaml:"homepage"`
	Featured    bool         `yaml:"featured"`
	Visibility  string       `yaml:"visibility"`
}

// Spec mirrors the app.yaml spec block.
type Spec struct {
	Chart ChartRef `yaml:"chart"`
	// TargetNamespace is the catalog's namespace template (e.g.
	// "{{.UserNamespace}}"). It is cloudbox-side templating metadata; the
	// peer-hosted install path instead takes an explicit, already-resolved
	// namespace from Target (see render.go) and never expands this string.
	TargetNamespace string `yaml:"targetNamespace"`
	RBAC            RBAC   `yaml:"rbac"`
	// DefaultValuesFile is the AUTHORITATIVE name of the sibling values
	// override (conventionally "values.yaml"). Empty means the app takes
	// no values override at all — a chart with sane defaults needs none.
	// When non-empty, Load resolves it as a bare filename sibling of
	// app.yaml (never a path — traversal and symlink escape are refused)
	// and requires it to exist: a catalog entry that DECLARES a values
	// file but does not ship it is a broken entry, not "no values". See
	// loadDeclaredValues.
	DefaultValuesFile string `yaml:"defaultValuesFile"`
}

// Manifest is the decoded, validated apps/<id>/app.yaml — the real
// dhnt/appstore AppEntry shape, decoded 1:1.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Load reads and validates entry's app manifest (apiVersion/kind envelope,
// id match, and chart shape — fail closed on any of them) plus the raw
// bytes of its declared values override (spec.defaultValuesFile — see
// loadDeclaredValues), returned UNMODIFIED (never templated or otherwise
// interpolated — see Render).
//
// There is deliberately no runtime license field to validate: license
// compatibility is enforced by dhnt/appstore CURATION (a public,
// OSS-only, reviewed catalog — see ../appstore/README.md), not by a field
// that the real catalog entries do not carry. Requiring one here would
// reject every genuine catalog app.
func Load(entry Entry) (*Manifest, string, error) {
	raw, err := os.ReadFile(entry.AppFile)
	if err != nil {
		return nil, "", fmt.Errorf("read app manifest %q: %w", entry.AppFile, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, "", fmt.Errorf("decode app manifest %q: %w", entry.AppFile, err)
	}
	if err := validateManifest(&m, entry.ID); err != nil {
		return nil, "", fmt.Errorf("app %q: %w", entry.ID, err)
	}

	valuesYAML, err := loadDeclaredValues(entry, &m)
	if err != nil {
		return nil, "", err
	}
	return &m, valuesYAML, nil
}

// loadDeclaredValues resolves spec.defaultValuesFile as the SOLE source of
// truth for which values override (if any) an app takes — the convention
// of "a file literally named values.yaml" is not consulted. Empty means
// no values, full stop. A non-empty value must be a bare filename (no "/",
// no ".." — never a path) and is resolved as a confined sibling of
// app.yaml via the same traversal/symlink-escape guard resolveAsset
// applies to app.yaml itself; required=true, so a DECLARED-but-missing
// file is a hard error rather than silently downgrading to "no values" —
// a catalog entry that names a file it doesn't ship is broken, not
// optional.
func loadDeclaredValues(entry Entry, m *Manifest) (string, error) {
	declared := strings.TrimSpace(m.Spec.DefaultValuesFile)
	if declared == "" {
		return "", nil
	}
	if filepath.Base(declared) != declared {
		return "", fmt.Errorf("app %q: spec.defaultValuesFile %q must be a bare filename, not a path", entry.ID, declared)
	}
	valuesPath, err := resolveAsset(entry.Catalog, entry.ID, declared, true)
	if err != nil {
		return "", fmt.Errorf("app %q: resolve spec.defaultValuesFile %q: %w", entry.ID, declared, err)
	}
	vraw, err := os.ReadFile(valuesPath)
	if err != nil {
		return "", fmt.Errorf("read values %q: %w", valuesPath, err)
	}
	return string(vraw), nil
}

// validateManifest fails closed on every unsupported dimension: a wrong
// apiVersion/kind envelope, a missing/mismatched metadata.id, or an
// incomplete/unsafe chart reference are all hard errors — never a
// best-effort partial install.
func validateManifest(m *Manifest, expectedID string) error {
	if av := strings.TrimSpace(m.APIVersion); av != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", m.APIVersion, APIVersion)
	}
	if k := strings.TrimSpace(m.Kind); k != Kind {
		return fmt.Errorf("unsupported kind %q (want %q)", m.Kind, Kind)
	}
	id := strings.TrimSpace(m.Metadata.ID)
	if id == "" || id != expectedID {
		return fmt.Errorf("manifest metadata.id %q does not match catalog entry %q", m.Metadata.ID, expectedID)
	}
	return validateChart(m.Spec.Chart)
}

// validateChart enforces a safe, complete chart reference: an https:// or
// oci:// repo URL with a host, a non-empty chart name, and an exact,
// injection-free version (no floating "latest").
func validateChart(c ChartRef) error {
	repo := strings.TrimSpace(c.Repo)
	if repo == "" {
		return fmt.Errorf("chart.repo is required")
	}
	u, err := url.Parse(repo)
	if err != nil {
		return fmt.Errorf("chart.repo %q is not a valid URL: %w", c.Repo, err)
	}
	if u.Scheme != "https" && u.Scheme != "oci" {
		return fmt.Errorf("chart.repo must be an https:// or oci:// URL, got %q", c.Repo)
	}
	if u.Host == "" {
		return fmt.Errorf("chart.repo %q has no host", c.Repo)
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("chart.name is required")
	}
	ver := strings.TrimSpace(c.Version)
	if ver == "" {
		return fmt.Errorf("chart.version is required (no floating \"latest\")")
	}
	if strings.EqualFold(ver, "latest") || !safeVersion.MatchString(ver) {
		return fmt.Errorf("chart.version %q is not a safe, pinned version", c.Version)
	}
	return nil
}
