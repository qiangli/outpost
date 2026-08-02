package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// `outpost appstore` — the "apps" half of the peer-DKS appstore parity
// surface: `outpost bundle install/status/uninstall` (bundle.go) covers
// the "builtins" half (raw builtin/<name>/install.yaml manifests); these
// subcommands resolve apps/<id>/app.yaml (+ optional values.yaml) and
// render a single k3s helm.cattle.io/v1 HelmChart custom resource — no
// Helm CLI, no manifest templating, no shell interpolation of any
// manifest/values field. Every invocation lands on the matching
// admincore.Appstore* method the MCP tools (tools_appstore.go) reach, so
// validation (the appstore.dhnt.io/v1 AppEntry envelope/id/chart shape +
// the cloudbox venue guard) cannot drift between surfaces.

func appstoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "appstore",
		Short: "Install/inspect OSS appstore apps (Helm charts) on a peer-hosted control plane (no cloudbox on the path)",
	}
	cmd.AddCommand(appstoreListCmd(), appstoreShowCmd(), appstoreInstallCmd(), appstoreStatusCmd(), appstoreUninstallCmd())
	return cmd
}

type appstoreCatalogOut struct {
	OK      bool     `json:"ok"`
	Catalog string   `json:"catalog"`
	Apps    []string `json:"apps"`
}

func appstoreListCmd() *cobra.Command {
	var offline bool
	var catalog string
	cmd := &cobra.Command{
		Use: "list", Short: "List installable OSS appstore app ids", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out appstoreCatalogOut
			if offline {
				core, err := offlineCore()
				if err != nil {
					return err
				}
				view, err := core.AppstoreCatalog(catalog)
				if err != nil {
					return err
				}
				out = appstoreCatalogOut{OK: view.OK, Catalog: view.Catalog, Apps: view.Apps}
			} else {
				session, err := dialMCP(cmd.Context())
				if err != nil {
					return err
				}
				defer session.close()
				args := map[string]any{}
				if strings.TrimSpace(catalog) != "" {
					args["catalog"] = strings.TrimSpace(catalog)
				}
				if err := session.callTool(cmd.Context(), "outpost_appstore_apps", args, &out); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "appstore_catalog: %s\napps:             %s\n", out.Catalog, strings.Join(out.Apps, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "Read the on-disk agent.json instead of the running daemon.")
	cmd.Flags().StringVar(&catalog, "catalog", "", "Explicit OSS dhnt/appstore root to inspect.")
	return cmd
}

type appstoreShowOut struct {
	OK            bool     `json:"ok"`
	ID            string   `json:"id"`
	Catalog       string   `json:"catalog"`
	Manifest      string   `json:"manifest"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Description   string   `json:"description"`
	Homepage      string   `json:"homepage"`
	Categories    []string `json:"categories"`
	Tags          []string `json:"tags"`
	Featured      bool     `json:"featured"`
	ChartRepo     string   `json:"chart_repo"`
	ChartName     string   `json:"chart_name"`
	ChartVersion  string   `json:"chart_version"`
	ClusterScoped bool     `json:"cluster_scoped"`
	HasValues     bool     `json:"has_values"`
}

func appstoreShowCmd() *cobra.Command {
	var offline bool
	var catalog string
	cmd := &cobra.Command{
		Use:   "show <app-id>",
		Short: "Show a resolved, validated OSS appstore app manifest (appstore.dhnt.io/v1 AppEntry) without touching any cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			var out appstoreShowOut
			if offline {
				core, err := offlineCore()
				if err != nil {
					return err
				}
				view, err := core.AppstoreShow(catalog, id)
				if err != nil {
					return err
				}
				out = appstoreShowOut{
					OK: view.OK, ID: view.ID, Catalog: view.Catalog, Manifest: view.Manifest,
					Name: view.Name, Version: view.Version, Description: view.Description, Homepage: view.Homepage,
					Categories: view.Categories, Tags: view.Tags, Featured: view.Featured,
					ChartRepo: view.ChartRepo, ChartName: view.ChartName, ChartVersion: view.ChartVersion,
					ClusterScoped: view.ClusterScoped, HasValues: view.HasValues,
				}
			} else {
				session, err := dialMCP(cmd.Context())
				if err != nil {
					return err
				}
				defer session.close()
				a := map[string]any{"id": id}
				if strings.TrimSpace(catalog) != "" {
					a["catalog"] = strings.TrimSpace(catalog)
				}
				if err := session.callTool(cmd.Context(), "outpost_appstore_app", a, &out); err != nil {
					return err
				}
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s: %s\n", out.ID, out.Name)
			if out.Version != "" {
				fmt.Fprintf(w, "version:  %s\n", out.Version)
			}
			fmt.Fprintf(w, "catalog:  %s\nmanifest: %s\n", out.Catalog, out.Manifest)
			if len(out.Categories) > 0 {
				fmt.Fprintf(w, "categories: %s\n", strings.Join(out.Categories, ", "))
			}
			if out.Homepage != "" {
				fmt.Fprintf(w, "homepage: %s\n", out.Homepage)
			}
			fmt.Fprintf(w, "chart:    %s (repo %s, version %s)\n", out.ChartName, out.ChartRepo, out.ChartVersion)
			fmt.Fprintf(w, "cluster-scoped: %v\n", out.ClusterScoped)
			fmt.Fprintf(w, "values:   %v\n", out.HasValues)
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "Read the on-disk agent.json instead of the running daemon.")
	cmd.Flags().StringVar(&catalog, "catalog", "", "Explicit OSS dhnt/appstore root to inspect.")
	return cmd
}

// appstoreTargetFlags is the namespace/release pair shared by
// install/status/uninstall — the per-user isolation coordinates.
type appstoreTargetFlags struct {
	namespace string
	release   string
}

func addAppstoreTargetFlags(cmd *cobra.Command, f *appstoreTargetFlags) {
	cmd.Flags().StringVar(&f.namespace, "namespace", "",
		"Target/user namespace the chart installs into (required — per-user isolation has no default).")
	cmd.Flags().StringVar(&f.release, "release", "",
		"Release name within namespace, for multiple instances of one app for one user. Defaults to the app id.")
}

type appstoreInstallFlags struct {
	appstoreTargetFlags
	kubeconfig       string
	catalog          string
	timeout          int
	poll             int
	allowScaleToZero bool
	noRollback       bool
	saveKubeconfig   bool
	saveCatalog      bool
	offline          bool
}

func resolveAppstoreInstallParams(id string, f *appstoreInstallFlags) admincore.AppstoreInstallParams {
	return admincore.AppstoreInstallParams{
		ID: id, Catalog: strings.TrimSpace(f.catalog), Kubeconfig: strings.TrimSpace(f.kubeconfig),
		Namespace: strings.TrimSpace(f.namespace), Release: strings.TrimSpace(f.release),
		TimeoutSeconds: f.timeout, PollSeconds: f.poll, AllowScaleToZero: f.allowScaleToZero,
		NoRollback: f.noRollback, SaveKubeconfig: f.saveKubeconfig, SaveCatalog: f.saveCatalog,
	}
}

func appstoreInstallArgs(p admincore.AppstoreInstallParams) map[string]any {
	args := map[string]any{"id": p.ID, "namespace": p.Namespace}
	if p.Catalog != "" {
		args["catalog"] = p.Catalog
	}
	if p.Kubeconfig != "" {
		args["kubeconfig"] = p.Kubeconfig
	}
	if p.Release != "" {
		args["release"] = p.Release
	}
	if p.TimeoutSeconds > 0 {
		args["timeout_seconds"] = p.TimeoutSeconds
	}
	if p.PollSeconds > 0 {
		args["poll_seconds"] = p.PollSeconds
	}
	if p.AllowScaleToZero {
		args["allow_scale_to_zero"] = true
	}
	if p.NoRollback {
		args["no_rollback"] = true
	}
	if p.SaveKubeconfig {
		args["save_kubeconfig"] = true
	}
	if p.SaveCatalog {
		args["save_catalog"] = true
	}
	return args
}

type appstoreInstallOut struct {
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

func appstoreInstallCmd() *cobra.Command {
	var f appstoreInstallFlags
	cmd := &cobra.Command{
		Use:   "install <app-id>",
		Short: "Install an OSS appstore app (a Helm chart) into your own namespace/release on a peer-hosted control plane",
		Long: "Resolves only apps/<id>/app.yaml (+ optional values.yaml); fails closed on an\n" +
			"unsupported apiVersion/kind envelope, a mismatched id, an unsafe chart\n" +
			"reference, or a name that is missing or escapes the catalog. Renders a single\n" +
			"k3s helm.cattle.io/v1 HelmChart object —\n" +
			"no Helm CLI, no manifest templating — and applies it through the same peer\n" +
			"readiness, rollback, and cloudbox-kubeconfig venue guard `bundle install` uses.\n" +
			"--namespace is required: per-user isolation has no default.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := resolveAppstoreInstallParams(args[0], &f)
			return runAppstoreInstall(cmd.Context(), p, f.offline, cmd.OutOrStdout())
		},
	}
	addAppstoreTargetFlags(cmd, &f.appstoreTargetFlags)
	cmd.Flags().StringVar(&f.kubeconfig, "kubeconfig", "",
		"Kubeconfig of the PEER control plane to apply against. Omit to use the persisted cluster.bundle_kubeconfig, else ~/.kube/outpost-control-plane/k3s.yaml. The cloudbox kubeconfig is refused.")
	cmd.Flags().StringVar(&f.catalog, "catalog", "", "OSS dhnt/appstore checkout (or fetched/versioned appstore tree).")
	cmd.Flags().IntVar(&f.timeout, "timeout", 0, "Bounded readiness wait, in seconds (default 300).")
	cmd.Flags().IntVar(&f.poll, "poll", 0, "Readiness poll interval, in seconds (default 2).")
	cmd.Flags().BoolVar(&f.allowScaleToZero, "allow-scale-to-zero", false,
		"Explicit opt-in: a workload with spec.replicas=0 counts as rolled out once drained.")
	cmd.Flags().BoolVar(&f.noRollback, "no-rollback", false,
		"On failure, leave objects this run created in place instead of rolling them back.")
	cmd.Flags().BoolVar(&f.saveKubeconfig, "save-kubeconfig", false, "After success, persist --kubeconfig as cluster.bundle_kubeconfig.")
	cmd.Flags().BoolVar(&f.saveCatalog, "save-catalog", false, "After success, persist --catalog as cluster.bundle_catalog.")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "Apply from this process using the on-disk agent.json — no running daemon required.")
	return cmd
}

func runAppstoreInstall(ctx context.Context, p admincore.AppstoreInstallParams, offline bool, w io.Writer) error {
	var out appstoreInstallOut
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		return appstoreInstallOffline(ctx, core, p, w)
	}
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	if err := session.callTool(ctx, "outpost_install_appstore_app", appstoreInstallArgs(p), &out); err != nil {
		return err
	}
	printAppstoreInstall(w, out)
	return nil
}

func appstoreInstallOffline(ctx context.Context, core *admincore.Server, p admincore.AppstoreInstallParams, w io.Writer) error {
	res, err := core.AppstoreInstall(ctx, p)
	if err != nil {
		return err
	}
	printAppstoreInstall(w, appstoreInstallOut{
		OK: res.OK, ID: res.ID, Catalog: res.Catalog, Manifest: res.Manifest,
		Namespace: res.Namespace, Release: res.Release, ObjectName: res.ObjectName,
		Kubeconfig: res.Kubeconfig, Applied: res.Applied, Ready: res.Ready,
		Created: res.Created, RolledBack: res.RolledBack, CleanupFailed: res.CleanupFailed,
		KubeconfigSaved: res.KubeconfigSaved, CatalogSaved: res.CatalogSaved,
	})
	return nil
}

func printAppstoreInstall(w io.Writer, out appstoreInstallOut) {
	fmt.Fprintf(w, "installed:  %s -> %s/%s (%d object(s), %d confirmed ready)\n", out.ID, out.Namespace, out.Release, out.Applied, out.Ready)
	fmt.Fprintf(w, "catalog:    %s\nmanifest:   %s\nkubeconfig: %s\nobject:     HelmChart kube-system/%s\n", out.Catalog, out.Manifest, out.Kubeconfig, out.ObjectName)
	if out.CatalogSaved {
		fmt.Fprintln(w, "saved cluster.bundle_catalog — future installs can omit --catalog")
	}
	if out.KubeconfigSaved {
		fmt.Fprintln(w, "saved cluster.bundle_kubeconfig — future installs can omit --kubeconfig")
	}
}

type appstoreStatusFlags struct {
	appstoreTargetFlags
	kubeconfig       string
	catalog          string
	allowScaleToZero bool
	offline          bool
}

func resolveAppstoreStatusParams(id string, f *appstoreStatusFlags) admincore.AppstoreStatusParams {
	return admincore.AppstoreStatusParams{
		ID: id, Catalog: strings.TrimSpace(f.catalog), Kubeconfig: strings.TrimSpace(f.kubeconfig),
		Namespace: strings.TrimSpace(f.namespace), Release: strings.TrimSpace(f.release),
		AllowScaleToZero: f.allowScaleToZero,
	}
}

func appstoreStatusArgs(p admincore.AppstoreStatusParams) map[string]any {
	args := map[string]any{"id": p.ID, "namespace": p.Namespace}
	if p.Catalog != "" {
		args["catalog"] = p.Catalog
	}
	if p.Kubeconfig != "" {
		args["kubeconfig"] = p.Kubeconfig
	}
	if p.Release != "" {
		args["release"] = p.Release
	}
	if p.AllowScaleToZero {
		args["allow_scale_to_zero"] = true
	}
	return args
}

type appstoreStatusOut struct {
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

func appstoreStatusCmd() *cobra.Command {
	var f appstoreStatusFlags
	cmd := &cobra.Command{
		Use:   "status <app-id>",
		Short: "Report whether an OSS appstore app is installed and ready in your namespace/release on a peer-hosted control plane",
		Long: "Read-only — applies nothing. Resolves apps/<id>/app.yaml exactly like\n" +
			"`appstore install` and recomputes the identical HelmChart object for the given\n" +
			"namespace/release, so installed/all_ready here means exactly what a successful\n" +
			"`appstore install` would have confirmed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := resolveAppstoreStatusParams(args[0], &f)
			return runAppstoreStatus(cmd.Context(), p, f.offline, cmd.OutOrStdout())
		},
	}
	addAppstoreTargetFlags(cmd, &f.appstoreTargetFlags)
	cmd.Flags().StringVar(&f.kubeconfig, "kubeconfig", "",
		"Kubeconfig of the PEER control plane to check. Omit to use the persisted cluster.bundle_kubeconfig, else ~/.kube/outpost-control-plane/k3s.yaml. The cloudbox kubeconfig is refused.")
	cmd.Flags().StringVar(&f.catalog, "catalog", "", "OSS dhnt/appstore checkout (or fetched/versioned appstore tree).")
	cmd.Flags().BoolVar(&f.allowScaleToZero, "allow-scale-to-zero", false,
		"Treat a spec.replicas=0 workload as ready (scaled down on purpose) instead of not-ready.")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "Check from this process using the on-disk agent.json — no running daemon required.")
	return cmd
}

func runAppstoreStatus(ctx context.Context, p admincore.AppstoreStatusParams, offline bool, w io.Writer) error {
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		return appstoreStatusOffline(ctx, core, p, w)
	}
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out appstoreStatusOut
	if err := session.callTool(ctx, "outpost_appstore_app_status", appstoreStatusArgs(p), &out); err != nil {
		return err
	}
	printAppstoreStatus(w, out)
	return nil
}

func appstoreStatusOffline(ctx context.Context, core *admincore.Server, p admincore.AppstoreStatusParams, w io.Writer) error {
	res, err := core.AppstoreStatus(ctx, p)
	if err != nil {
		return err
	}
	printAppstoreStatus(w, appstoreStatusOut{
		OK: res.OK, ID: res.ID, Catalog: res.Catalog, Manifest: res.Manifest,
		Namespace: res.Namespace, Release: res.Release, ObjectName: res.ObjectName,
		Kubeconfig: res.Kubeconfig, Installed: res.Installed, AllReady: res.AllReady, Reason: res.Reason,
	})
	return nil
}

func printAppstoreStatus(w io.Writer, out appstoreStatusOut) {
	fmt.Fprintf(w, "%s @ %s/%s: installed=%v all_ready=%v\n", out.ID, out.Namespace, out.Release, out.Installed, out.AllReady)
	fmt.Fprintf(w, "catalog:    %s\nmanifest:   %s\nkubeconfig: %s\nobject:     HelmChart kube-system/%s\n", out.Catalog, out.Manifest, out.Kubeconfig, out.ObjectName)
	if out.Reason != "" {
		fmt.Fprintf(w, "reason:     %s\n", out.Reason)
	}
}

type appstoreUninstallFlags struct {
	appstoreTargetFlags
	kubeconfig string
	catalog    string
	timeout    int
	poll       int
	offline    bool
}

func resolveAppstoreUninstallParams(id string, f *appstoreUninstallFlags) admincore.AppstoreUninstallParams {
	return admincore.AppstoreUninstallParams{
		ID: id, Catalog: strings.TrimSpace(f.catalog), Kubeconfig: strings.TrimSpace(f.kubeconfig),
		Namespace: strings.TrimSpace(f.namespace), Release: strings.TrimSpace(f.release),
		TimeoutSeconds: f.timeout, PollSeconds: f.poll,
	}
}

func appstoreUninstallArgs(p admincore.AppstoreUninstallParams) map[string]any {
	args := map[string]any{"id": p.ID, "namespace": p.Namespace}
	if p.Catalog != "" {
		args["catalog"] = p.Catalog
	}
	if p.Kubeconfig != "" {
		args["kubeconfig"] = p.Kubeconfig
	}
	if p.Release != "" {
		args["release"] = p.Release
	}
	if p.TimeoutSeconds > 0 {
		args["timeout_seconds"] = p.TimeoutSeconds
	}
	if p.PollSeconds > 0 {
		args["poll_seconds"] = p.PollSeconds
	}
	return args
}

type appstoreUninstallOut struct {
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

func appstoreUninstallCmd() *cobra.Command {
	var f appstoreUninstallFlags
	cmd := &cobra.Command{
		Use:   "uninstall <app-id>",
		Short: "Remove an OSS appstore app's HelmChart release from your namespace on a peer-hosted control plane",
		Long: "Resolves apps/<id>/app.yaml exactly like `appstore install` and removes the\n" +
			"single HelmChart object for the given namespace/release — deleting it is what\n" +
			"tells the k3s helm-controller to `helm uninstall` the release. The target\n" +
			"namespace itself is left alone (it may host other apps for the same user). The\n" +
			"cloudbox kubeconfig is always refused, same venue guard as install/status.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := resolveAppstoreUninstallParams(args[0], &f)
			return runAppstoreUninstall(cmd.Context(), p, f.offline, cmd.OutOrStdout())
		},
	}
	addAppstoreTargetFlags(cmd, &f.appstoreTargetFlags)
	cmd.Flags().StringVar(&f.kubeconfig, "kubeconfig", "",
		"Kubeconfig of the PEER control plane to uninstall from. Omit to use the persisted cluster.bundle_kubeconfig, else ~/.kube/outpost-control-plane/k3s.yaml. The cloudbox kubeconfig is refused.")
	cmd.Flags().StringVar(&f.catalog, "catalog", "", "OSS dhnt/appstore checkout (or fetched/versioned appstore tree).")
	cmd.Flags().IntVar(&f.timeout, "timeout", 0, "Bounded wait for the deleted object to actually vanish, in seconds. 0 (default) skips the wait.")
	cmd.Flags().IntVar(&f.poll, "poll", 0, "Gone-check poll interval, in seconds (default 2).")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "Uninstall from this process using the on-disk agent.json — no running daemon required.")
	return cmd
}

func runAppstoreUninstall(ctx context.Context, p admincore.AppstoreUninstallParams, offline bool, w io.Writer) error {
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		return appstoreUninstallOffline(ctx, core, p, w)
	}
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out appstoreUninstallOut
	if err := session.callTool(ctx, "outpost_uninstall_appstore_app", appstoreUninstallArgs(p), &out); err != nil {
		return err
	}
	printAppstoreUninstall(w, out)
	return nil
}

func appstoreUninstallOffline(ctx context.Context, core *admincore.Server, p admincore.AppstoreUninstallParams, w io.Writer) error {
	res, err := core.AppstoreUninstall(ctx, p)
	if err != nil {
		return err
	}
	printAppstoreUninstall(w, appstoreUninstallOut{
		OK: res.OK, ID: res.ID, Catalog: res.Catalog, Manifest: res.Manifest,
		Namespace: res.Namespace, Release: res.Release, ObjectName: res.ObjectName,
		Kubeconfig: res.Kubeconfig, Deleted: res.Deleted, Failed: res.Failed, Gone: res.Gone,
	})
	return nil
}

func printAppstoreUninstall(w io.Writer, out appstoreUninstallOut) {
	fmt.Fprintf(w, "uninstalled: %s from %s/%s (%d object(s) removed)\n", out.ID, out.Namespace, out.Release, len(out.Deleted))
	fmt.Fprintf(w, "catalog:     %s\nmanifest:    %s\nkubeconfig:  %s\n", out.Catalog, out.Manifest, out.Kubeconfig)
	if len(out.Failed) > 0 {
		fmt.Fprintf(w, "left behind (remove by hand): %s\n", strings.Join(out.Failed, ", "))
	}
}
