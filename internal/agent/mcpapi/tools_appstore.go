package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Peer-DKS appstore APPS tools — the "apps" half of the peer parity
// surface against cloudbox's DKS appstore. tools_bundle.go covers the
// "builtins" half (raw builtin/<name>/install.yaml manifests); these
// tools resolve apps/<id>/app.yaml (+ optional values.yaml) and render a
// single k3s helm.cattle.io/v1 HelmChart custom resource — no Helm CLI,
// no shell interpolation of any manifest/values field. Every tool is a
// thin wrapper over the matching admincore.Appstore* method, so
// validation (schema/license/platform + the cloudbox venue guard) cannot
// drift between the MCP, CLI, and offline paths.

type appstoreCatalogIn struct {
	Catalog string `json:"catalog,omitempty" jsonschema:"explicit OSS appstore root; omit to use cluster.bundle_catalog or umbrella-checkout discovery"`
}

type appstoreCatalogOut struct {
	OK      bool     `json:"ok"`
	Catalog string   `json:"catalog"`
	Apps    []string `json:"apps"`
}

type appstoreShowIn struct {
	ID      string `json:"id" jsonschema:"operator-facing OSS appstore app id, e.g. cert-manager"`
	Catalog string `json:"catalog,omitempty" jsonschema:"path to an OSS dhnt/appstore checkout or fetched/versioned appstore tree; omit to use cluster.bundle_catalog or umbrella-checkout discovery"`
}

type appstoreShowOut struct {
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

type installAppstoreAppIn struct {
	ID               string `json:"id" jsonschema:"operator-facing OSS appstore app id, e.g. cert-manager"`
	Catalog          string `json:"catalog,omitempty" jsonschema:"path to an OSS dhnt/appstore checkout or fetched/versioned appstore tree; omit to use cluster.bundle_catalog or umbrella-checkout discovery"`
	Kubeconfig       string `json:"kubeconfig,omitempty" jsonschema:"kubeconfig path of the PEER control plane; the cloudbox kubeconfig is refused"`
	Namespace        string `json:"namespace" jsonschema:"target/user namespace the chart installs into (required — per-user isolation has no default)"`
	Release          string `json:"release,omitempty" jsonschema:"release name within namespace, for multiple instances of one app for one user; defaults to id"`
	TimeoutSeconds   int    `json:"timeout_seconds,omitempty"`
	PollSeconds      int    `json:"poll_seconds,omitempty"`
	AllowScaleToZero bool   `json:"allow_scale_to_zero,omitempty"`
	NoRollback       bool   `json:"no_rollback,omitempty"`
	SaveKubeconfig   bool   `json:"save_kubeconfig,omitempty"`
	SaveCatalog      bool   `json:"save_catalog,omitempty" jsonschema:"after a successful install, persist the explicit catalog as cluster.bundle_catalog"`
}

type installAppstoreAppOut struct {
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

type appstoreAppStatusIn struct {
	ID               string `json:"id" jsonschema:"operator-facing OSS appstore app id"`
	Catalog          string `json:"catalog,omitempty" jsonschema:"path to an OSS dhnt/appstore checkout or fetched/versioned appstore tree; omit to use cluster.bundle_catalog or umbrella-checkout discovery"`
	Kubeconfig       string `json:"kubeconfig,omitempty" jsonschema:"kubeconfig path of the PEER control plane; the cloudbox kubeconfig is refused"`
	Namespace        string `json:"namespace" jsonschema:"target/user namespace to check (required)"`
	Release          string `json:"release,omitempty" jsonschema:"release name within namespace; defaults to id"`
	AllowScaleToZero bool   `json:"allow_scale_to_zero,omitempty"`
}

type appstoreAppStatusOut struct {
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

type uninstallAppstoreAppIn struct {
	ID             string `json:"id" jsonschema:"operator-facing OSS appstore app id to remove"`
	Catalog        string `json:"catalog,omitempty" jsonschema:"path to an OSS dhnt/appstore checkout or fetched/versioned appstore tree; omit to use cluster.bundle_catalog or umbrella-checkout discovery"`
	Kubeconfig     string `json:"kubeconfig,omitempty" jsonschema:"kubeconfig path of the PEER control plane; the cloudbox kubeconfig is refused"`
	Namespace      string `json:"namespace" jsonschema:"target/user namespace to remove from (required)"`
	Release        string `json:"release,omitempty" jsonschema:"release name within namespace; defaults to id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	PollSeconds    int    `json:"poll_seconds,omitempty"`
}

type uninstallAppstoreAppOut struct {
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

func (s *Server) registerAppstoreTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_appstore_apps",
		Description: "List the OSS appstore app ids (Helm charts, apps/<id>/app.yaml) currently installable from the effective catalog.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in appstoreCatalogIn) (*mcp.CallToolResult, appstoreCatalogOut, error) {
		view, err := s.core.AppstoreCatalog(in.Catalog)
		if err != nil {
			return apiErrResult[appstoreCatalogOut](err)
		}
		return nil, appstoreCatalogOut{OK: view.OK, Catalog: view.Catalog, Apps: view.Apps}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_appstore_app",
		Description: "Show a resolved, VALIDATED OSS appstore app manifest — schema version, license, and platform support checked exactly as " +
			"install would check them — without touching any cluster or requiring a kubeconfig.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in appstoreShowIn) (*mcp.CallToolResult, appstoreShowOut, error) {
		view, err := s.core.AppstoreShow(in.Catalog, in.ID)
		if err != nil {
			return apiErrResult[appstoreShowOut](err)
		}
		return nil, appstoreShowOut{
			OK: view.OK, ID: view.ID, Catalog: view.Catalog, Manifest: view.Manifest,
			DisplayName: view.DisplayName, License: view.License, Platforms: view.Platforms,
			ChartRepo: view.ChartRepo, ChartName: view.ChartName, ChartVersion: view.ChartVersion,
			HasValues: view.HasValues,
		}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_install_appstore_app",
		Description: "Install an operator-named OSS appstore app (a Helm chart) onto a peer-hosted DKS plane, into the caller's own " +
			"namespace/release. Resolves only apps/<id>/app.yaml (+ optional values.yaml); fails closed on an unsupported schema version, " +
			"license, or platform, or a name that is missing or escapes the catalog. Renders a single k3s helm.cattle.io/v1 HelmChart " +
			"object — no Helm CLI, no manifest templating — and applies it through the same peer readiness, rollback, and " +
			"cloudbox-kubeconfig venue guard the builtins path uses.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in installAppstoreAppIn) (*mcp.CallToolResult, installAppstoreAppOut, error) {
		res, err := s.core.AppstoreInstall(ctx, admincore.AppstoreInstallParams{
			ID: in.ID, Catalog: in.Catalog, Kubeconfig: in.Kubeconfig,
			Namespace: in.Namespace, Release: in.Release,
			TimeoutSeconds: in.TimeoutSeconds, PollSeconds: in.PollSeconds,
			AllowScaleToZero: in.AllowScaleToZero, NoRollback: in.NoRollback,
			SaveKubeconfig: in.SaveKubeconfig, SaveCatalog: in.SaveCatalog,
		})
		if err != nil {
			return apiErrResult[installAppstoreAppOut](err)
		}
		return nil, installAppstoreAppOut{
			OK: res.OK, ID: res.ID, Catalog: res.Catalog, Manifest: res.Manifest,
			Namespace: res.Namespace, Release: res.Release, ObjectName: res.ObjectName,
			Kubeconfig: res.Kubeconfig, Applied: res.Applied, Ready: res.Ready,
			Created: res.Created, RolledBack: res.RolledBack, CleanupFailed: res.CleanupFailed,
			KubeconfigSaved: res.KubeconfigSaved, CatalogSaved: res.CatalogSaved,
		}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_appstore_app_status",
		Description: "Report the live install state of an operator-named OSS appstore app on a peer-hosted DKS plane — read-only, applies " +
			"nothing. Resolves apps/<id>/app.yaml exactly like outpost_install_appstore_app and recomputes the identical HelmChart object " +
			"for the given namespace/release, so installed=true/all_ready=true means exactly what a successful install would have confirmed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in appstoreAppStatusIn) (*mcp.CallToolResult, appstoreAppStatusOut, error) {
		res, err := s.core.AppstoreStatus(ctx, admincore.AppstoreStatusParams{
			ID: in.ID, Catalog: in.Catalog, Kubeconfig: in.Kubeconfig,
			Namespace: in.Namespace, Release: in.Release, AllowScaleToZero: in.AllowScaleToZero,
		})
		if err != nil {
			return apiErrResult[appstoreAppStatusOut](err)
		}
		return nil, appstoreAppStatusOut{
			OK: res.OK, ID: res.ID, Catalog: res.Catalog, Manifest: res.Manifest,
			Namespace: res.Namespace, Release: res.Release, ObjectName: res.ObjectName,
			Kubeconfig: res.Kubeconfig, Installed: res.Installed, AllReady: res.AllReady, Reason: res.Reason,
		}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_uninstall_appstore_app",
		Description: "Remove an operator-named OSS appstore app's HelmChart CR — and therefore its Helm release — from a peer-hosted DKS " +
			"plane. Resolves apps/<id>/app.yaml exactly like outpost_install_appstore_app and recomputes the identical object for the given " +
			"namespace/release. The target namespace itself is left alone (it may host other apps for the same user).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uninstallAppstoreAppIn) (*mcp.CallToolResult, uninstallAppstoreAppOut, error) {
		res, err := s.core.AppstoreUninstall(ctx, admincore.AppstoreUninstallParams{
			ID: in.ID, Catalog: in.Catalog, Kubeconfig: in.Kubeconfig,
			Namespace: in.Namespace, Release: in.Release,
			TimeoutSeconds: in.TimeoutSeconds, PollSeconds: in.PollSeconds,
		})
		if err != nil {
			return apiErrResult[uninstallAppstoreAppOut](err)
		}
		return nil, uninstallAppstoreAppOut{
			OK: res.OK, ID: res.ID, Catalog: res.Catalog, Manifest: res.Manifest,
			Namespace: res.Namespace, Release: res.Release, ObjectName: res.ObjectName,
			Kubeconfig: res.Kubeconfig, Deleted: res.Deleted, Failed: res.Failed, Gone: res.Gone,
		}, nil
	})
}
