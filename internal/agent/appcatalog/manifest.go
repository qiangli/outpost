package appcatalog

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only apps/<id>/app.yaml schema version this
// package understands. Any other value — including a missing/zero one —
// fails closed; there is no best-effort parse of an unknown shape.
const SchemaVersion = 1

// supportedLicenses is the allowlist of SPDX license identifiers this
// package will install. It keeps the peer-DKS apps path scoped to
// unambiguous, redistributable open-source licenses — the same
// "no proprietary manifests" boundary builtincatalog enforces
// structurally by only ever reading from an OSS appstore checkout. An
// app.yaml declaring any other (or no) license fails closed.
var supportedLicenses = map[string]bool{
	"Apache-2.0": true,
	"MIT":        true,

	"BSD-2-Clause": true,
	"BSD-3-Clause": true,
	"0BSD":         true,

	"MPL-2.0":   true,
	"ISC":       true,
	"Unlicense": true,

	"GPL-2.0": true, "GPL-2.0-only": true, "GPL-2.0-or-later": true,
	"GPL-3.0": true, "GPL-3.0-only": true, "GPL-3.0-or-later": true,
	"LGPL-2.1": true, "LGPL-2.1-only": true, "LGPL-2.1-or-later": true,
	"LGPL-3.0": true, "LGPL-3.0-only": true, "LGPL-3.0-or-later": true,
	"AGPL-3.0": true, "AGPL-3.0-only": true, "AGPL-3.0-or-later": true,
}

// supportedPlatforms is the fixed set of node platforms a peer-hosted DKS
// (k3s) control plane can schedule containers onto — the compute half is
// always a Linux container runtime, regardless of the host OS the
// outpost daemon itself happens to run on (a macOS outpost administers a
// Linux k3s-agent container; see internal/agent/runtime). An app.yaml
// that does not declare compatibility with at least one of these is
// unsupported for this venue and refused, independent of what OS/arch
// the daemon process performing the install happens to be running on.
var supportedPlatforms = map[string]bool{
	"linux/amd64": true,
	"linux/arm64": true,
}

// ChartRef names the Helm chart an app.yaml installs.
type ChartRef struct {
	// Repo is the Helm chart repository URL. Must be https:// or oci://
	// — never a bare string that could be shell-interpreted.
	Repo string `yaml:"repo"`
	// Name is the chart name within Repo.
	Name string `yaml:"name"`
	// Version pins an exact chart version — no floating "latest".
	Version string `yaml:"version"`
}

// Manifest is the decoded, validated apps/<id>/app.yaml.
type Manifest struct {
	SchemaVersion int      `yaml:"schemaVersion"`
	ID            string   `yaml:"id"`
	DisplayName   string   `yaml:"displayName"`
	License       string   `yaml:"license"`
	Platforms     []string `yaml:"platforms"`
	Chart         ChartRef `yaml:"chart"`
}

// Load reads and validates entry's app manifest (schema version,
// license, platform, and chart shape — fail closed on any of them) plus
// the raw bytes of its optional values.yaml, returned UNMODIFIED (never
// templated or otherwise interpolated — see Render).
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

	var valuesYAML string
	if entry.ValuesFile != "" {
		vraw, err := os.ReadFile(entry.ValuesFile)
		if err != nil {
			return nil, "", fmt.Errorf("read values %q: %w", entry.ValuesFile, err)
		}
		valuesYAML = string(vraw)
	}
	return &m, valuesYAML, nil
}

// validateManifest fails closed on every unsupported dimension per the
// evidence invariant: an unrecognized schema version, a missing/
// non-allowlisted license, a platform list that doesn't include a peer
// DKS node platform, or an incomplete chart reference are all hard
// errors — never a best-effort partial install.
func validateManifest(m *Manifest, expectedID string) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported app schema version %d (supported: %d)", m.SchemaVersion, SchemaVersion)
	}
	id := strings.TrimSpace(m.ID)
	if id == "" || id != expectedID {
		return fmt.Errorf("manifest id %q does not match catalog entry %q", m.ID, expectedID)
	}
	if !supportedLicenses[strings.TrimSpace(m.License)] {
		return fmt.Errorf("unsupported or missing license %q", m.License)
	}
	if len(m.Platforms) == 0 {
		return fmt.Errorf("declares no supported platforms")
	}
	supported := false
	for _, p := range m.Platforms {
		if supportedPlatforms[strings.TrimSpace(p)] {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("declared platforms %v are unsupported on a peer-hosted DKS (Linux) plane", m.Platforms)
	}
	repo := strings.TrimSpace(m.Chart.Repo)
	if !strings.HasPrefix(repo, "https://") && !strings.HasPrefix(repo, "oci://") {
		return fmt.Errorf("chart.repo must be an https:// or oci:// URL, got %q", m.Chart.Repo)
	}
	if strings.TrimSpace(m.Chart.Name) == "" {
		return fmt.Errorf("chart.name is required")
	}
	if strings.TrimSpace(m.Chart.Version) == "" {
		return fmt.Errorf("chart.version is required (no floating \"latest\")")
	}
	return nil
}
