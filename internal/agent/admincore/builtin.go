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

// BuiltinInstall resolves only <appstore>/builtin/<name>/install.yaml. It does
// not consult cloudbox or accept an arbitrary manifest path; BundleApply then
// enforces the cloudbox venue guard, readiness gates, and rollback semantics.
func (s *Server) BuiltinInstall(ctx context.Context, p BuiltinInstallParams) (BuiltinInstallResult, error) {
	if p.SaveCatalog && strings.TrimSpace(p.Catalog) == "" {
		return BuiltinInstallResult{}, badRequest("save_catalog needs an explicit catalog to save")
	}
	s.mu.Lock()
	fc, err := s.loadConfig()
	if err != nil {
		s.mu.Unlock()
		return BuiltinInstallResult{}, err
	}
	catalog := strings.TrimSpace(p.Catalog)
	if catalog == "" && fc.Cluster != nil {
		catalog = strings.TrimSpace(fc.Cluster.BundleCatalog)
	}
	s.mu.Unlock()

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
	s.mu.Lock()
	fc, err := s.loadConfig()
	if err != nil {
		s.mu.Unlock()
		return BundleCatalogView{}, err
	}
	catalog := strings.TrimSpace(explicit)
	if catalog == "" && fc.Cluster != nil {
		catalog = strings.TrimSpace(fc.Cluster.BundleCatalog)
	}
	s.mu.Unlock()
	root, names, err := builtincatalog.List(catalog)
	if err != nil {
		return BundleCatalogView{}, badRequest("%s", err.Error())
	}
	return BundleCatalogView{OK: true, Catalog: root, Builtins: names}, nil
}
