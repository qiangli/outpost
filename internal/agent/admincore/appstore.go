package admincore

import (
	"context"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/qiangli/outpost/internal/agent/appcatalog"
	"github.com/qiangli/outpost/internal/agent/bundleapply"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// Peer-DKS appstore APPS — the "apps" half of the peer parity surface
// against cloudbox's DKS appstore (builtin.go / bundle.go cover the
// "builtins" half: raw builtin/<name>/install.yaml manifests). An app
// names a Helm chart via apps/<id>/app.yaml (+ optional
// apps/<id>/values.yaml), resolved through the identical catalog
// confinement builtincatalog uses (see internal/agent/appcatalog), and
// rendered into a single k3s helm.cattle.io/v1 HelmChart custom
// resource — never a shelled-out Helm CLI, never a templated manifest.
// The k3s helm-controller (built into every peer-hosted DKS control
// plane) performs the actual `helm upgrade --install` in-cluster from
// the object's structured spec fields.
//
// The rendered HelmChart is a one-object bundleapply.Bundle, so install /
// status / uninstall reuse ApplyBundle / StatusBundle / DeleteBundle
// directly — the same ordering, readiness, rollback, and deletion
// mechanics BundleApply / BundleStatus / BundleUninstall already give the
// builtins path — and resolveBundleVenue gives the identical
// cloudbox-kubeconfig venue guard. Namespace/release isolation is
// enforced by appcatalog.Target: Namespace is REQUIRED (no default), so
// two users — or two instances for one user — can never collide on the
// same rendered object.

// effectiveAppCatalog is effectiveCatalog under a name that reads clearly
// at the appstore-apps call sites; it resolves the identical persisted
// cluster.bundle_catalog default the builtins path uses; a peer-hosted
// appstore checkout normally ships both trees (builtin/ and apps/) under
// one root.
func (s *Server) effectiveAppCatalog(explicit string) (string, error) {
	return s.effectiveCatalog(explicit)
}

// appTarget resolves the required Namespace + optional Release (default:
// the app id) into an appcatalog.Target. Namespace is never defaulted —
// per-user isolation has no safe guess.
func appTarget(id, namespace, release string) appcatalog.Target {
	rel := strings.TrimSpace(release)
	if rel == "" {
		rel = strings.TrimSpace(id)
	}
	return appcatalog.Target{Namespace: strings.TrimSpace(namespace), Release: rel}
}

// AppstoreCatalogView reports the effective catalog and its installable
// app ids.
type AppstoreCatalogView struct {
	OK      bool     `json:"ok"`
	Catalog string   `json:"catalog"`
	Apps    []string `json:"apps"`
}

// AppstoreCatalog lists the app ids installable from the effective
// catalog (explicit param, else persisted cluster.bundle_catalog).
func (s *Server) AppstoreCatalog(explicit string) (AppstoreCatalogView, error) {
	catalog, err := s.effectiveAppCatalog(explicit)
	if err != nil {
		return AppstoreCatalogView{}, err
	}
	root, ids, err := appcatalog.List(catalog)
	if err != nil {
		return AppstoreCatalogView{}, badRequest("%s", err.Error())
	}
	return AppstoreCatalogView{OK: true, Catalog: root, Apps: ids}, nil
}

// AppstoreShowResult previews a resolved, VALIDATED app manifest without
// touching any cluster — schema version, license, and platform support
// are checked here exactly as install would check them, so an operator
// can see WHY an app is unsupported before ever naming a kubeconfig.
type AppstoreShowResult struct {
	OK           bool     `json:"ok"`
	ID           string   `json:"id"`
	Catalog      string   `json:"catalog"`
	Manifest     string   `json:"manifest"`
	DisplayName  string   `json:"display_name,omitempty"`
	License      string   `json:"license"`
	Platforms    []string `json:"platforms"`
	ChartRepo    string   `json:"chart_repo"`
	ChartName    string   `json:"chart_name"`
	ChartVersion string   `json:"chart_version"`
	HasValues    bool     `json:"has_values"`
}

// AppstoreShow resolves and validates apps/<id>/app.yaml the same way
// AppstoreInstall would, and reports its metadata. Read-only — nothing
// is applied, and no kubeconfig is required.
func (s *Server) AppstoreShow(explicitCatalog, id string) (AppstoreShowResult, error) {
	catalog, err := s.effectiveAppCatalog(explicitCatalog)
	if err != nil {
		return AppstoreShowResult{}, err
	}
	entry, err := appcatalog.Resolve(catalog, id)
	if err != nil {
		return AppstoreShowResult{}, badRequest("%s", err.Error())
	}
	m, values, err := appcatalog.Load(entry)
	if err != nil {
		return AppstoreShowResult{}, badRequest("%s", err.Error())
	}
	return AppstoreShowResult{
		OK: true, ID: entry.ID, Catalog: entry.Catalog, Manifest: entry.AppFile,
		DisplayName: m.DisplayName, License: m.License, Platforms: m.Platforms,
		ChartRepo: m.Chart.Repo, ChartName: m.Chart.Name, ChartVersion: m.Chart.Version,
		HasValues: strings.TrimSpace(values) != "",
	}, nil
}

// AppstoreInstallParams installs one operator-named OSS appstore app
// (a Helm chart, rendered to a HelmChart CR) into the caller's own
// namespace/release on a peer-hosted DKS plane.
type AppstoreInstallParams struct {
	ID               string `json:"id"`
	Catalog          string `json:"catalog,omitempty"`
	Kubeconfig       string `json:"kubeconfig,omitempty"`
	Namespace        string `json:"namespace"`
	Release          string `json:"release,omitempty"`
	TimeoutSeconds   int    `json:"timeout_seconds,omitempty"`
	PollSeconds      int    `json:"poll_seconds,omitempty"`
	AllowScaleToZero bool   `json:"allow_scale_to_zero,omitempty"`
	NoRollback       bool   `json:"no_rollback,omitempty"`
	SaveKubeconfig   bool   `json:"save_kubeconfig,omitempty"`
	SaveCatalog      bool   `json:"save_catalog,omitempty"`
}

// AppstoreInstallResult reports one appstore app install run.
type AppstoreInstallResult struct {
	OK              bool     `json:"ok"`
	ID              string   `json:"id"`
	Catalog         string   `json:"catalog"`
	Manifest        string   `json:"manifest"`
	Namespace       string   `json:"namespace"`
	Release         string   `json:"release"`
	ObjectName      string   `json:"object_name"`
	Kubeconfig      string   `json:"kubeconfig"`
	Applied         int      `json:"applied"`
	Ready           int      `json:"ready"`
	Created         []string `json:"created,omitempty"`
	RolledBack      []string `json:"rolled_back,omitempty"`
	CleanupFailed   []string `json:"cleanup_failed,omitempty"`
	KubeconfigSaved bool     `json:"kubeconfig_saved,omitempty"`
	CatalogSaved    bool     `json:"catalog_saved,omitempty"`
}

// AppstoreInstall resolves apps/<id>/app.yaml (+ optional values.yaml),
// fails closed on an unsupported schema version, license, or platform,
// renders the single HelmChart object for the caller's namespace/release,
// and applies it through the same peer venue guard, readiness wait, and
// rollback accounting BundleApply gives the builtins path.
func (s *Server) AppstoreInstall(ctx context.Context, p AppstoreInstallParams) (AppstoreInstallResult, error) {
	if p.SaveCatalog && strings.TrimSpace(p.Catalog) == "" {
		return AppstoreInstallResult{}, badRequest("save_catalog needs an explicit catalog to save")
	}
	if p.SaveKubeconfig && strings.TrimSpace(p.Kubeconfig) == "" {
		return AppstoreInstallResult{}, badRequest("save_kubeconfig needs an explicit kubeconfig to save")
	}
	catalog, err := s.effectiveAppCatalog(p.Catalog)
	if err != nil {
		return AppstoreInstallResult{}, err
	}
	entry, err := appcatalog.Resolve(catalog, p.ID)
	if err != nil {
		return AppstoreInstallResult{}, badRequest("%s", err.Error())
	}
	manifest, values, err := appcatalog.Load(entry)
	if err != nil {
		return AppstoreInstallResult{}, badRequest("%s", err.Error())
	}

	target := appTarget(entry.ID, p.Namespace, p.Release)
	obj, err := appcatalog.Render(manifest, values, target)
	if err != nil {
		return AppstoreInstallResult{}, badRequest("%s", err.Error())
	}

	canonical, client, err := s.resolveBundleVenue(p.Kubeconfig)
	if err != nil {
		return AppstoreInstallResult{}, err
	}

	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if p.TimeoutSeconds <= 0 {
		timeout = 5 * time.Minute
	}
	poll := time.Duration(p.PollSeconds) * time.Second

	res, applyErr := bundleapply.ApplyBundle(ctx, &bundleapply.Bundle{Objects: []*unstructured.Unstructured{obj}}, bundleapply.Options{
		Client:           client,
		Timeout:          timeout,
		PollInterval:     poll,
		AllowScaleToZero: p.AllowScaleToZero,
		DisableRollback:  p.NoRollback,
	})
	out := AppstoreInstallResult{
		OK: applyErr == nil, ID: entry.ID, Catalog: entry.Catalog, Manifest: entry.AppFile,
		Namespace: target.Namespace, Release: target.Release, ObjectName: obj.GetName(),
		Kubeconfig: canonical, Applied: res.Applied, Ready: res.Ready,
		Created: res.Created, RolledBack: res.RolledBack, CleanupFailed: res.CleanupFailed,
	}
	if applyErr != nil {
		return out, applyErr
	}

	// Persist only AFTER a successful apply — a saved default that never
	// worked would be a trap for the next run (same rule BundleApply /
	// BuiltinInstall follow).
	if p.SaveKubeconfig || p.SaveCatalog {
		s.mu.Lock()
		defer s.mu.Unlock()
		fc, err := s.loadConfig()
		if err != nil {
			return out, err
		}
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		if p.SaveKubeconfig {
			fc.Cluster.BundleKubeconfig = strings.TrimSpace(p.Kubeconfig)
		}
		if p.SaveCatalog {
			fc.Cluster.BundleCatalog = entry.Catalog
		}
		if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
			return out, internalErr("app installed, but saving catalog/kubeconfig default failed: %s", err.Error())
		}
		out.KubeconfigSaved = p.SaveKubeconfig
		out.CatalogSaved = p.SaveCatalog
	}
	return out, nil
}

// AppstoreStatusParams resolves one operator-named OSS appstore app and
// reports its live state on a peer-hosted plane — read-only.
type AppstoreStatusParams struct {
	ID               string `json:"id"`
	Catalog          string `json:"catalog,omitempty"`
	Kubeconfig       string `json:"kubeconfig,omitempty"`
	Namespace        string `json:"namespace"`
	Release          string `json:"release,omitempty"`
	AllowScaleToZero bool   `json:"allow_scale_to_zero,omitempty"`
}

// AppstoreStatusResult reports the resolved app plus its HelmChart CR's
// live state.
type AppstoreStatusResult struct {
	OK         bool   `json:"ok"`
	ID         string `json:"id"`
	Catalog    string `json:"catalog"`
	Manifest   string `json:"manifest"`
	Namespace  string `json:"namespace"`
	Release    string `json:"release"`
	ObjectName string `json:"object_name"`
	Kubeconfig string `json:"kubeconfig"`
	Installed  bool   `json:"installed"`
	AllReady   bool   `json:"all_ready"`
	Reason     string `json:"reason,omitempty"`
}

// AppstoreStatus resolves apps/<id>/app.yaml exactly the way
// AppstoreInstall does (same fail-closed schema/license/platform check)
// and recomputes the identical deterministic HelmChart object name for
// the given namespace/release, then reports its live state — reusing the
// same readiness evidence (bundleapply.StatusBundle -> evalReadiness)
// ApplyBundle's wait uses, so installed=true/all_ready=true means exactly
// what a successful AppstoreInstall would have confirmed.
func (s *Server) AppstoreStatus(ctx context.Context, p AppstoreStatusParams) (AppstoreStatusResult, error) {
	catalog, err := s.effectiveAppCatalog(p.Catalog)
	if err != nil {
		return AppstoreStatusResult{}, err
	}
	entry, err := appcatalog.Resolve(catalog, p.ID)
	if err != nil {
		return AppstoreStatusResult{}, badRequest("%s", err.Error())
	}
	if _, _, err := appcatalog.Load(entry); err != nil {
		return AppstoreStatusResult{}, badRequest("%s", err.Error())
	}

	target := appTarget(entry.ID, p.Namespace, p.Release)
	obj, err := appcatalog.ReleaseObject(target)
	if err != nil {
		return AppstoreStatusResult{}, badRequest("%s", err.Error())
	}

	canonical, client, err := s.resolveBundleVenue(p.Kubeconfig)
	if err != nil {
		return AppstoreStatusResult{}, err
	}

	res, err := bundleapply.StatusBundle(ctx, &bundleapply.Bundle{Objects: []*unstructured.Unstructured{obj}}, bundleapply.StatusOptions{
		Client: client, AllowScaleToZero: p.AllowScaleToZero,
	})
	if err != nil {
		return AppstoreStatusResult{}, internalErr("%s", err.Error())
	}
	out := AppstoreStatusResult{
		OK: true, ID: entry.ID, Catalog: entry.Catalog, Manifest: entry.AppFile,
		Namespace: target.Namespace, Release: target.Release, ObjectName: obj.GetName(),
		Kubeconfig: canonical, Installed: res.Installed, AllReady: res.AllReady,
	}
	if len(res.Objects) == 1 {
		out.Reason = res.Objects[0].Reason
	}
	return out, nil
}

// AppstoreUninstallParams resolves one operator-named OSS appstore app
// and removes its HelmChart CR from a peer-hosted plane.
type AppstoreUninstallParams struct {
	ID             string `json:"id"`
	Catalog        string `json:"catalog,omitempty"`
	Kubeconfig     string `json:"kubeconfig,omitempty"`
	Namespace      string `json:"namespace"`
	Release        string `json:"release,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	PollSeconds    int    `json:"poll_seconds,omitempty"`
}

// AppstoreUninstallResult reports one uninstall run.
type AppstoreUninstallResult struct {
	OK         bool     `json:"ok"`
	ID         string   `json:"id"`
	Catalog    string   `json:"catalog"`
	Manifest   string   `json:"manifest"`
	Namespace  string   `json:"namespace"`
	Release    string   `json:"release"`
	ObjectName string   `json:"object_name"`
	Kubeconfig string   `json:"kubeconfig"`
	Deleted    []string `json:"deleted,omitempty"`
	Failed     []string `json:"failed,omitempty"`
	Gone       int      `json:"gone,omitempty"`
}

// AppstoreUninstall resolves apps/<id>/app.yaml exactly the way
// AppstoreInstall does, recomputes the identical HelmChart object for
// the given namespace/release, and removes it — deleting the HelmChart
// CR is what tells the k3s helm-controller to `helm uninstall` the
// release. The caller's target namespace is left alone: it may be
// hosting other apps for the same user (per-user namespace isolation is
// about SHARING a namespace across a user's installs, not owning it
// exclusively), so uninstall never deletes it.
func (s *Server) AppstoreUninstall(ctx context.Context, p AppstoreUninstallParams) (AppstoreUninstallResult, error) {
	catalog, err := s.effectiveAppCatalog(p.Catalog)
	if err != nil {
		return AppstoreUninstallResult{}, err
	}
	entry, err := appcatalog.Resolve(catalog, p.ID)
	if err != nil {
		return AppstoreUninstallResult{}, badRequest("%s", err.Error())
	}
	if _, _, err := appcatalog.Load(entry); err != nil {
		return AppstoreUninstallResult{}, badRequest("%s", err.Error())
	}

	target := appTarget(entry.ID, p.Namespace, p.Release)
	obj, err := appcatalog.ReleaseObject(target)
	if err != nil {
		return AppstoreUninstallResult{}, badRequest("%s", err.Error())
	}

	canonical, client, err := s.resolveBundleVenue(p.Kubeconfig)
	if err != nil {
		return AppstoreUninstallResult{}, err
	}

	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	poll := time.Duration(p.PollSeconds) * time.Second
	res, delErr := bundleapply.DeleteBundle(ctx, &bundleapply.Bundle{Objects: []*unstructured.Unstructured{obj}}, bundleapply.DeleteOptions{
		Client: client, Timeout: timeout, PollInterval: poll,
	})
	out := AppstoreUninstallResult{
		OK: delErr == nil, ID: entry.ID, Catalog: entry.Catalog, Manifest: entry.AppFile,
		Namespace: target.Namespace, Release: target.Release, ObjectName: obj.GetName(),
		Kubeconfig: canonical, Deleted: res.Deleted, Failed: res.Failed, Gone: res.Gone,
	}
	if delErr != nil {
		return out, delErr
	}
	return out, nil
}
