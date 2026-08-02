package admincore

import (
	"context"
	"strings"

	"github.com/qiangli/outpost/internal/agent/builtincatalog"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// BuiltinInstallParams installs one operator-named OSS appstore built-in by
// feeding its resolved manifest into the peer-only BundleApply transaction.
type BuiltinInstallParams struct {
	Name              string `json:"name"`
	Catalog           string `json:"catalog,omitempty"`
	Kubeconfig        string `json:"kubeconfig,omitempty"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty"`
	PollSeconds       int    `json:"poll_seconds,omitempty"`
	CRDTimeoutSeconds int    `json:"crd_timeout_seconds,omitempty"`
	AllowScaleToZero  bool   `json:"allow_scale_to_zero,omitempty"`
	NoRollback        bool   `json:"no_rollback,omitempty"`
	SaveKubeconfig    bool   `json:"save_kubeconfig,omitempty"`
	SaveCatalog       bool   `json:"save_catalog,omitempty"`
}

type BuiltinInstallResult struct {
	BundleApplyResult
	Name         string `json:"name"`
	Catalog      string `json:"catalog"`
	Manifest     string `json:"manifest"`
	CatalogSaved bool   `json:"catalog_saved,omitempty"`
}

type BundleCatalogView struct {
	OK       bool     `json:"ok"`
	Catalog  string   `json:"catalog"`
	Builtins []string `json:"builtins"`
}

// effectiveCatalog resolves the catalog to search: an explicit param wins,
// else the persisted cluster.bundle_catalog. Shared by every operation
// that resolves an operator-facing built-in name (install, status,
// uninstall, and the catalog listing) so the persisted default can never
// drift between them.
func (s *Server) effectiveCatalog(explicit string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	catalog := strings.TrimSpace(explicit)
	if catalog == "" && fc.Cluster != nil {
		catalog = strings.TrimSpace(fc.Cluster.BundleCatalog)
	}
	return catalog, nil
}

// BuiltinInstall resolves only <appstore>/builtin/<name>/install.yaml. It does
// not consult cloudbox or accept an arbitrary manifest path; BundleApply then
// enforces the cloudbox venue guard, readiness gates, and rollback semantics.
func (s *Server) BuiltinInstall(ctx context.Context, p BuiltinInstallParams) (BuiltinInstallResult, error) {
	if p.SaveCatalog && strings.TrimSpace(p.Catalog) == "" {
		return BuiltinInstallResult{}, badRequest("save_catalog needs an explicit catalog to save")
	}
	catalog, err := s.effectiveCatalog(p.Catalog)
	if err != nil {
		return BuiltinInstallResult{}, err
	}

	entry, err := builtincatalog.Resolve(catalog, p.Name)
	if err != nil {
		return BuiltinInstallResult{}, badRequest("%s", err.Error())
	}
	apply, err := s.BundleApply(ctx, BundleApplyParams{
		Kubeconfig: p.Kubeconfig, Bundle: entry.Manifest,
		TimeoutSeconds: p.TimeoutSeconds, PollSeconds: p.PollSeconds,
		CRDTimeoutSeconds: p.CRDTimeoutSeconds, AllowScaleToZero: p.AllowScaleToZero,
		NoRollback: p.NoRollback, SaveKubeconfig: p.SaveKubeconfig,
	})
	out := BuiltinInstallResult{BundleApplyResult: apply, Name: entry.Name, Catalog: entry.Catalog, Manifest: entry.Manifest}
	if err != nil {
		return out, err
	}
	if p.SaveCatalog {
		s.mu.Lock()
		defer s.mu.Unlock()
		fc, err := s.loadConfig()
		if err != nil {
			return out, err
		}
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		fc.Cluster.BundleCatalog = entry.Catalog
		if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
			return out, internalErr("built-in installed, but saving bundle_catalog failed: %s", err.Error())
		}
		out.CatalogSaved = true
	}
	return out, nil
}

// BundleCatalog reports the effective catalog and its installable names.
func (s *Server) BundleCatalog(explicit string) (BundleCatalogView, error) {
	catalog, err := s.effectiveCatalog(explicit)
	if err != nil {
		return BundleCatalogView{}, err
	}
	root, names, err := builtincatalog.List(catalog)
	if err != nil {
		return BundleCatalogView{}, badRequest("%s", err.Error())
	}
	return BundleCatalogView{OK: true, Catalog: root, Builtins: names}, nil
}

// BuiltinStatusParams resolves one operator-named OSS appstore built-in
// and reports its live state on a peer-hosted plane — read-only, applies
// nothing.
type BuiltinStatusParams struct {
	Name             string `json:"name"`
	Catalog          string `json:"catalog,omitempty"`
	Kubeconfig       string `json:"kubeconfig,omitempty"`
	AllowScaleToZero bool   `json:"allow_scale_to_zero,omitempty"`
}

// BuiltinStatusResult reports the resolved built-in plus its bundle-wide
// status snapshot.
type BuiltinStatusResult struct {
	BundleStatusResult
	Name     string `json:"name"`
	Catalog  string `json:"catalog"`
	Manifest string `json:"manifest"`
}

// BuiltinStatus resolves <catalog>/builtin/<name>/install.yaml exactly
// the way BuiltinInstall does, then delegates to BundleStatus — so
// "is this built-in installed and ready" always answers against the same
// manifest BuiltinInstall would apply, through the same venue guard.
func (s *Server) BuiltinStatus(ctx context.Context, p BuiltinStatusParams) (BuiltinStatusResult, error) {
	catalog, err := s.effectiveCatalog(p.Catalog)
	if err != nil {
		return BuiltinStatusResult{}, err
	}
	entry, err := builtincatalog.Resolve(catalog, p.Name)
	if err != nil {
		return BuiltinStatusResult{}, badRequest("%s", err.Error())
	}
	status, err := s.BundleStatus(ctx, BundleStatusParams{
		Kubeconfig: p.Kubeconfig, Bundle: entry.Manifest, AllowScaleToZero: p.AllowScaleToZero,
	})
	out := BuiltinStatusResult{BundleStatusResult: status, Name: entry.Name, Catalog: entry.Catalog, Manifest: entry.Manifest}
	if err != nil {
		return out, err
	}
	return out, nil
}

// BuiltinUninstallParams resolves one operator-named OSS appstore
// built-in and removes it from a peer-hosted plane.
type BuiltinUninstallParams struct {
	Name           string `json:"name"`
	Catalog        string `json:"catalog,omitempty"`
	Kubeconfig     string `json:"kubeconfig,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	PollSeconds    int    `json:"poll_seconds,omitempty"`
}

// BuiltinUninstallResult reports the resolved built-in plus its uninstall
// accounting.
type BuiltinUninstallResult struct {
	BundleUninstallResult
	Name     string `json:"name"`
	Catalog  string `json:"catalog"`
	Manifest string `json:"manifest"`
}

// BuiltinUninstall resolves only <appstore>/builtin/<name>/install.yaml —
// the identical resolution BuiltinInstall uses — and delegates to
// BundleUninstall, so removing a built-in reuses the exact venue guard
// and deletion mechanics BuiltinInstall's rollback path already relies
// on. It does not consult cloudbox or accept an arbitrary manifest path.
func (s *Server) BuiltinUninstall(ctx context.Context, p BuiltinUninstallParams) (BuiltinUninstallResult, error) {
	catalog, err := s.effectiveCatalog(p.Catalog)
	if err != nil {
		return BuiltinUninstallResult{}, err
	}
	entry, err := builtincatalog.Resolve(catalog, p.Name)
	if err != nil {
		return BuiltinUninstallResult{}, badRequest("%s", err.Error())
	}
	res, err := s.BundleUninstall(ctx, BundleUninstallParams{
		Kubeconfig: p.Kubeconfig, Bundle: entry.Manifest, TimeoutSeconds: p.TimeoutSeconds, PollSeconds: p.PollSeconds,
	})
	out := BuiltinUninstallResult{BundleUninstallResult: res, Name: entry.Name, Catalog: entry.Catalog, Manifest: entry.Manifest}
	if err != nil {
		return out, err
	}
	return out, nil
}
