package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Peer-DKS bundle apply tools. Arg names match the agent.json key
// (cluster.bundle_kubeconfig → "kubeconfig" scoped by the tool name, plus
// the operation parameters). Both tools are thin wrappers over
// admincore.BundleApply / BundleKubeconfig — the same methods the CLI and
// the operator script's Go runner reach, so validation (venue guard
// included) cannot drift between surfaces.
//
// NO CLOUDBOX ON THIS PATH: the apply reads a kubeconfig file and talks
// to the apiserver named in it. The venue guard refuses the cloudbox
// kubeconfig (~/.kube/outpost.yaml) after canonicalization, symlinked or
// relative routes included.

type applyBundleIn struct {
	Kubeconfig        string `json:"kubeconfig,omitempty" jsonschema:"kubeconfig path of the PEER control plane to apply against. Omit to use the persisted cluster.bundle_kubeconfig, else the conventional ~/.kube/outpost-control-plane/k3s.yaml. The cloudbox kubeconfig is refused after symlink-resolving canonicalization."`
	Bundle            string `json:"bundle" jsonschema:"manifest file or directory of manifests to apply (required)"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty" jsonschema:"bounded readiness wait for the whole bundle; default 300"`
	PollSeconds       int    `json:"poll_seconds,omitempty" jsonschema:"readiness poll interval; default 2"`
	CRDTimeoutSeconds int    `json:"crd_timeout_seconds,omitempty" jsonschema:"bounded per-CRD wait for Established + discovery before dependent custom resources apply; default 60"`
	AllowScaleToZero  bool   `json:"allow_scale_to_zero,omitempty" jsonschema:"explicit opt-in: a workload with spec.replicas=0 counts as rolled out once drained; without it zero-desired is a hard failure"`
	NoRollback        bool   `json:"no_rollback,omitempty" jsonschema:"on failure, leave objects this run created in place (still reported precisely) instead of rolling them back"`
	SaveKubeconfig    bool   `json:"save_kubeconfig,omitempty" jsonschema:"after a successful apply, persist the explicit kubeconfig as cluster.bundle_kubeconfig (Live - no restart)"`
}

type applyBundleOut struct {
	OK              bool     `json:"ok"`
	Kubeconfig      string   `json:"kubeconfig"`
	Applied         int      `json:"applied"`
	Ready           int      `json:"ready"`
	Created         []string `json:"created,omitempty"`
	RolledBack      []string `json:"rolled_back,omitempty"`
	CleanupFailed   []string `json:"cleanup_failed,omitempty"`
	KubeconfigSaved bool     `json:"kubeconfig_saved,omitempty"`
}

type bundleKubeconfigOut struct {
	OK         bool   `json:"ok"`
	Kubeconfig string `json:"kubeconfig"`
	Default    string `json:"default"`
}

type installBuiltinIn struct {
	Name              string `json:"name" jsonschema:"operator-facing OSS appstore built-in name, for example core-storage, coredns, headlamp, or gpu-device-plugin"`
	Catalog           string `json:"catalog,omitempty" jsonschema:"path to an OSS dhnt/appstore checkout or fetched/versioned appstore tree; omit to use cluster.bundle_catalog or umbrella-checkout discovery"`
	Kubeconfig        string `json:"kubeconfig,omitempty" jsonschema:"kubeconfig path of the PEER control plane; the cloudbox kubeconfig is refused"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty"`
	PollSeconds       int    `json:"poll_seconds,omitempty"`
	CRDTimeoutSeconds int    `json:"crd_timeout_seconds,omitempty"`
	AllowScaleToZero  bool   `json:"allow_scale_to_zero,omitempty"`
	NoRollback        bool   `json:"no_rollback,omitempty"`
	SaveKubeconfig    bool   `json:"save_kubeconfig,omitempty"`
	SaveCatalog       bool   `json:"save_catalog,omitempty" jsonschema:"after a successful install, persist the explicit catalog as cluster.bundle_catalog"`
}

type installBuiltinOut struct {
	OK              bool     `json:"ok"`
	Name            string   `json:"name"`
	Catalog         string   `json:"catalog"`
	Manifest        string   `json:"manifest"`
	Kubeconfig      string   `json:"kubeconfig"`
	Applied         int      `json:"applied"`
	Ready           int      `json:"ready"`
	Created         []string `json:"created,omitempty"`
	RolledBack      []string `json:"rolled_back,omitempty"`
	CleanupFailed   []string `json:"cleanup_failed,omitempty"`
	KubeconfigSaved bool     `json:"kubeconfig_saved,omitempty"`
	CatalogSaved    bool     `json:"catalog_saved,omitempty"`
}

type bundleCatalogOut struct {
	OK       bool     `json:"ok"`
	Catalog  string   `json:"catalog"`
	Builtins []string `json:"builtins"`
}

type bundleCatalogIn struct {
	Catalog string `json:"catalog,omitempty" jsonschema:"explicit OSS appstore root; omit to use cluster.bundle_catalog or umbrella-checkout discovery"`
}

func (s *Server) registerBundleTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_install_builtin",
		Description: "Install an operator-named built-in from an open-source dhnt/appstore catalog onto a peer-hosted DKS plane. " +
			"Only builtin/<name>/install.yaml is resolved; missing, invalid, or escaping assets fail closed. " +
			"The install reuses the peer bundle readiness, rollback, and cloudbox-kubeconfig venue guard.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in installBuiltinIn) (*mcp.CallToolResult, installBuiltinOut, error) {
		res, err := s.core.BuiltinInstall(ctx, admincore.BuiltinInstallParams{
			Name: in.Name, Catalog: in.Catalog, Kubeconfig: in.Kubeconfig,
			TimeoutSeconds: in.TimeoutSeconds, PollSeconds: in.PollSeconds,
			CRDTimeoutSeconds: in.CRDTimeoutSeconds, AllowScaleToZero: in.AllowScaleToZero,
			NoRollback: in.NoRollback, SaveKubeconfig: in.SaveKubeconfig, SaveCatalog: in.SaveCatalog,
		})
		if err != nil {
			return apiErrResult[installBuiltinOut](err)
		}
		return nil, installBuiltinOut{
			OK: res.OK, Name: res.Name, Catalog: res.Catalog, Manifest: res.Manifest,
			Kubeconfig: res.Kubeconfig, Applied: res.Applied, Ready: res.Ready,
			Created: res.Created, RolledBack: res.RolledBack, CleanupFailed: res.CleanupFailed,
			KubeconfigSaved: res.KubeconfigSaved, CatalogSaved: res.CatalogSaved,
		}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_bundle_catalog",
		Description: "Show the effective open-source appstore catalog and the built-in names currently installable from it.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in bundleCatalogIn) (*mcp.CallToolResult, bundleCatalogOut, error) {
		view, err := s.core.BundleCatalog(in.Catalog)
		if err != nil {
			return apiErrResult[bundleCatalogOut](err)
		}
		return nil, bundleCatalogOut{OK: view.OK, Catalog: view.Catalog, Builtins: view.Builtins}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_apply_bundle",
		Description: "Apply an app/bundle manifest set against a PEER-HOSTED control plane addressed purely by a kubeconfig path — no cloudbox anywhere on the path. " +
			"Applies in prerequisite order (Namespaces/CRDs/RBAC before workloads), gates custom resources behind their CRD reaching Established AND discovery, " +
			"then waits (bounded) for every workload to be ROLLED OUT: updated replicas, StatefulSet revision parity, zero-desired refused without allow_scale_to_zero. " +
			"On failure it rolls back exactly what this run created (reverse order, best-effort, reported precisely) unless no_rollback. " +
			"The cloudbox kubeconfig (~/.kube/outpost.yaml) is refused after canonicalization — symlinked or relative spellings included.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in applyBundleIn) (*mcp.CallToolResult, applyBundleOut, error) {
		res, err := s.core.BundleApply(ctx, admincore.BundleApplyParams{
			Kubeconfig:        in.Kubeconfig,
			Bundle:            in.Bundle,
			TimeoutSeconds:    in.TimeoutSeconds,
			PollSeconds:       in.PollSeconds,
			CRDTimeoutSeconds: in.CRDTimeoutSeconds,
			AllowScaleToZero:  in.AllowScaleToZero,
			NoRollback:        in.NoRollback,
			SaveKubeconfig:    in.SaveKubeconfig,
		})
		if err != nil {
			return apiErrResult[applyBundleOut](err)
		}
		return nil, applyBundleOut{
			OK:              res.OK,
			Kubeconfig:      res.Kubeconfig,
			Applied:         res.Applied,
			Ready:           res.Ready,
			Created:         res.Created,
			RolledBack:      res.RolledBack,
			CleanupFailed:   res.CleanupFailed,
			KubeconfigSaved: res.KubeconfigSaved,
		}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_bundle_kubeconfig",
		Description: "Report the persisted default kubeconfig venue for peer bundle applies (cluster.bundle_kubeconfig) and the conventional " +
			"peer control-plane default used when nothing is persisted. A path, not a credential — nothing sensitive is returned.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, bundleKubeconfigOut, error) {
		view, err := s.core.BundleKubeconfig()
		if err != nil {
			return apiErrResult[bundleKubeconfigOut](err)
		}
		return nil, bundleKubeconfigOut{OK: view.OK, Kubeconfig: view.Kubeconfig, Default: view.Default}, nil
	})
}
