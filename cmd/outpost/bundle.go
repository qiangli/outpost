package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// `outpost bundle` — peer-DKS bundle/app apply, the CLI face of
// admincore.BundleApply.
//
// Every invocation lands on the same admincore method the MCP tool
// (`outpost_apply_bundle`) and the operator script's Go runner reach, so
// the venue guard, the rolled-out readiness bar, and the rollback
// accounting cannot drift between surfaces. The default path is a thin
// MCP client (the daemon does the apply); `--offline` runs the apply in
// this process against the on-disk config — no daemon required, which is
// the CLI twin of `script/dks-peer-bundle-apply.sh`.
//
// NO CLOUDBOX ON THIS PATH: the apply reads a kubeconfig file and talks
// to the apiserver named in it. The cloudbox kubeconfig
// (~/.kube/outpost.yaml) is refused after symlink-resolving
// canonicalization, however it is spelled.

// bundleApplyFlags is the flag set shared by the MCP and --offline paths.
type bundleApplyFlags struct {
	kubeconfig       string
	timeout          int
	poll             int
	crdTimeout       int
	allowScaleToZero bool
	noRollback       bool
	saveKubeconfig   bool
	offline          bool
}

func addBundleApplyFlags(cmd *cobra.Command, f *bundleApplyFlags) {
	cmd.Flags().StringVar(&f.kubeconfig, "kubeconfig", "",
		"Kubeconfig of the PEER control plane to apply against. Omit to use the persisted cluster.bundle_kubeconfig, else ~/.kube/outpost-control-plane/k3s.yaml. The cloudbox kubeconfig is refused.")
	cmd.Flags().IntVar(&f.timeout, "timeout", 0,
		"Bounded readiness wait for the whole bundle, in seconds (default 300).")
	cmd.Flags().IntVar(&f.poll, "poll", 0,
		"Readiness poll interval, in seconds (default 2).")
	cmd.Flags().IntVar(&f.crdTimeout, "crd-timeout", 0,
		"Bounded per-CRD wait for Established + discovery before dependent custom resources apply, in seconds (default 60).")
	cmd.Flags().BoolVar(&f.allowScaleToZero, "allow-scale-to-zero", false,
		"Explicit opt-in: a workload with spec.replicas=0 counts as rolled out once drained. Without it, zero-desired is a hard failure.")
	cmd.Flags().BoolVar(&f.noRollback, "no-rollback", false,
		"On failure, leave objects this run created in place (still reported precisely) instead of rolling them back.")
	cmd.Flags().BoolVar(&f.saveKubeconfig, "save-kubeconfig", false,
		"After a successful apply, persist the explicit --kubeconfig as cluster.bundle_kubeconfig (Live — no restart).")
	cmd.Flags().BoolVar(&f.offline, "offline", false,
		"Apply from this process using the on-disk agent.json — no running daemon required.")
}

// resolveBundleApplyParams turns argv + flags into the one admincore
// request shape. Both the MCP and --offline paths run through this, so
// what the operator typed maps to admincore.BundleApply identically.
func resolveBundleApplyParams(bundle string, f *bundleApplyFlags) admincore.BundleApplyParams {
	return admincore.BundleApplyParams{
		Kubeconfig:        strings.TrimSpace(f.kubeconfig),
		Bundle:            bundle,
		TimeoutSeconds:    f.timeout,
		PollSeconds:       f.poll,
		CRDTimeoutSeconds: f.crdTimeout,
		AllowScaleToZero:  f.allowScaleToZero,
		NoRollback:        f.noRollback,
		SaveKubeconfig:    f.saveKubeconfig,
	}
}

// bundleApplyArgs builds the MCP argument map. Keys match the
// outpost_apply_bundle tool's arg names (which follow the agent.json
// naming convention); zero values are omitted so the daemon's defaults
// apply.
func bundleApplyArgs(p admincore.BundleApplyParams) map[string]any {
	args := map[string]any{"bundle": p.Bundle}
	if p.Kubeconfig != "" {
		args["kubeconfig"] = p.Kubeconfig
	}
	if p.TimeoutSeconds > 0 {
		args["timeout_seconds"] = p.TimeoutSeconds
	}
	if p.PollSeconds > 0 {
		args["poll_seconds"] = p.PollSeconds
	}
	if p.CRDTimeoutSeconds > 0 {
		args["crd_timeout_seconds"] = p.CRDTimeoutSeconds
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
	return args
}

// bundleApplyOut mirrors the outpost_apply_bundle result (and
// admincore.BundleApplyResult) over MCP.
type bundleApplyOut struct {
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

func bundleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Apply app/bundle manifests to a peer-hosted control plane (no cloudbox on the path)",
	}
	cmd.AddCommand(bundleApplyCmd(), bundleInstallCmd(), bundleKubeconfigCmd(), bundleCatalogCmd())
	return cmd
}

type bundleInstallFlags struct {
	bundleApplyFlags
	catalog     string
	saveCatalog bool
}

func resolveBuiltinInstallParams(name string, f *bundleInstallFlags) admincore.BuiltinInstallParams {
	return admincore.BuiltinInstallParams{
		Name: name, Catalog: strings.TrimSpace(f.catalog), Kubeconfig: strings.TrimSpace(f.kubeconfig),
		TimeoutSeconds: f.timeout, PollSeconds: f.poll, CRDTimeoutSeconds: f.crdTimeout,
		AllowScaleToZero: f.allowScaleToZero, NoRollback: f.noRollback,
		SaveKubeconfig: f.saveKubeconfig, SaveCatalog: f.saveCatalog,
	}
}

func builtinInstallArgs(p admincore.BuiltinInstallParams) map[string]any {
	args := map[string]any{"name": p.Name}
	if p.Catalog != "" {
		args["catalog"] = p.Catalog
	}
	if p.Kubeconfig != "" {
		args["kubeconfig"] = p.Kubeconfig
	}
	if p.TimeoutSeconds > 0 {
		args["timeout_seconds"] = p.TimeoutSeconds
	}
	if p.PollSeconds > 0 {
		args["poll_seconds"] = p.PollSeconds
	}
	if p.CRDTimeoutSeconds > 0 {
		args["crd_timeout_seconds"] = p.CRDTimeoutSeconds
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

type builtinInstallOut struct {
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

func bundleInstallCmd() *cobra.Command {
	var f bundleInstallFlags
	cmd := &cobra.Command{
		Use:   "install <built-in>",
		Short: "Install an OSS appstore built-in on a peer-hosted control plane",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := resolveBuiltinInstallParams(args[0], &f)
			return runBuiltinInstall(cmd.Context(), p, f.offline, cmd.OutOrStdout())
		},
	}
	addBundleApplyFlags(cmd, &f.bundleApplyFlags)
	cmd.Flags().StringVar(&f.catalog, "catalog", "", "OSS dhnt/appstore checkout (or fetched/versioned appstore tree).")
	cmd.Flags().BoolVar(&f.saveCatalog, "save-catalog", false, "After success, persist explicit --catalog as cluster.bundle_catalog.")
	return cmd
}

func runBuiltinInstall(ctx context.Context, p admincore.BuiltinInstallParams, offline bool, w io.Writer) error {
	var out builtinInstallOut
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		return builtinInstallOffline(ctx, core, p, w)
	} else {
		session, err := dialMCP(ctx)
		if err != nil {
			return err
		}
		defer session.close()
		if err := session.callTool(ctx, "outpost_install_builtin", builtinInstallArgs(p), &out); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "installed:  %s (%d object(s), %d confirmed ready)\n", out.Name, out.Applied, out.Ready)
	fmt.Fprintf(w, "catalog:    %s\nmanifest:   %s\nkubeconfig: %s\n", out.Catalog, out.Manifest, out.Kubeconfig)
	if out.CatalogSaved {
		fmt.Fprintln(w, "saved cluster.bundle_catalog — future installs can omit --catalog")
	}
	if out.KubeconfigSaved {
		fmt.Fprintln(w, "saved cluster.bundle_kubeconfig — future installs can omit --kubeconfig")
	}
	return nil
}

func builtinInstallOffline(ctx context.Context, core *admincore.Server, p admincore.BuiltinInstallParams, w io.Writer) error {
	res, err := core.BuiltinInstall(ctx, p)
	if err != nil {
		return err
	}
	out := builtinInstallOut{
		OK: res.OK, Name: res.Name, Catalog: res.Catalog, Manifest: res.Manifest,
		Kubeconfig: res.Kubeconfig, Applied: res.Applied, Ready: res.Ready,
		Created: res.Created, RolledBack: res.RolledBack, CleanupFailed: res.CleanupFailed,
		KubeconfigSaved: res.KubeconfigSaved, CatalogSaved: res.CatalogSaved,
	}
	fmt.Fprintf(w, "installed:  %s (%d object(s), %d confirmed ready)\n", out.Name, out.Applied, out.Ready)
	fmt.Fprintf(w, "catalog:    %s\nmanifest:   %s\nkubeconfig: %s\n", out.Catalog, out.Manifest, out.Kubeconfig)
	if out.CatalogSaved {
		fmt.Fprintln(w, "saved cluster.bundle_catalog — future installs can omit --catalog")
	}
	if out.KubeconfigSaved {
		fmt.Fprintln(w, "saved cluster.bundle_kubeconfig — future installs can omit --kubeconfig")
	}
	return nil
}

type bundleCatalogOut struct {
	OK       bool     `json:"ok"`
	Catalog  string   `json:"catalog"`
	Builtins []string `json:"builtins"`
}

func bundleCatalogCmd() *cobra.Command {
	var offline bool
	var catalog string
	cmd := &cobra.Command{
		Use: "catalog", Short: "Show the effective OSS catalog and installable built-ins", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out bundleCatalogOut
			if offline {
				core, err := offlineCore()
				if err != nil {
					return err
				}
				view, err := core.BundleCatalog(catalog)
				if err != nil {
					return err
				}
				out = bundleCatalogOut{OK: view.OK, Catalog: view.Catalog, Builtins: view.Builtins}
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
				if err := session.callTool(cmd.Context(), "outpost_bundle_catalog", args, &out); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "bundle_catalog: %s\nbuilt-ins:     %s\n", out.Catalog, strings.Join(out.Builtins, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "Read the on-disk agent.json instead of the running daemon.")
	cmd.Flags().StringVar(&catalog, "catalog", "", "Explicit OSS dhnt/appstore root to inspect.")
	return cmd
}

func bundleApplyCmd() *cobra.Command {
	var f bundleApplyFlags
	cmd := &cobra.Command{
		Use:   "apply <manifest-file-or-dir>",
		Short: "Apply a bundle and wait (bounded) for every workload to be ROLLED OUT",
		Long: "Applies the bundle in prerequisite order (Namespaces/CRDs/RBAC before workloads),\n" +
			"gates custom resources behind their CRD reaching Established AND discovery, then\n" +
			"waits — bounded — for every workload to be rolled out: updated replicas, StatefulSet\n" +
			"revision parity, zero-desired refused without --allow-scale-to-zero. On failure it\n" +
			"rolls back exactly what this run created and reports what was and was not cleaned.\n" +
			"The kubeconfig venue is canonicalized (symlinks resolved); the cloudbox kubeconfig\n" +
			"(~/.kube/outpost.yaml) is always refused.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := resolveBundleApplyParams(args[0], &f)
			return runBundleApply(cmd.Context(), p, f.offline, cmd.OutOrStdout())
		},
	}
	addBundleApplyFlags(cmd, &f)
	return cmd
}

func runBundleApply(ctx context.Context, p admincore.BundleApplyParams, offline bool, w io.Writer) error {
	if offline {
		core, err := offlineCore()
		if err != nil {
			return err
		}
		return bundleApplyOffline(ctx, core, p, w)
	}
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out bundleApplyOut
	if err := session.callTool(ctx, "outpost_apply_bundle", bundleApplyArgs(p), &out); err != nil {
		return err
	}
	printBundleApply(w, out)
	return nil
}

// bundleApplyOffline is the daemon-free path: the SAME admincore method
// the MCP tool reaches, called in-process. Split out so the parity test
// can drive it against an injected fake cluster.
func bundleApplyOffline(ctx context.Context, core *admincore.Server, p admincore.BundleApplyParams, w io.Writer) error {
	res, err := core.BundleApply(ctx, p)
	if err != nil {
		return err
	}
	printBundleApply(w, bundleApplyOut{
		OK: res.OK, Kubeconfig: res.Kubeconfig, Applied: res.Applied, Ready: res.Ready,
		Created: res.Created, RolledBack: res.RolledBack, CleanupFailed: res.CleanupFailed,
		KubeconfigSaved: res.KubeconfigSaved,
	})
	return nil
}

func printBundleApply(w io.Writer, out bundleApplyOut) {
	fmt.Fprintf(w, "applied:    %d object(s), %d confirmed ready (rolled out)\n", out.Applied, out.Ready)
	fmt.Fprintf(w, "kubeconfig: %s\n", out.Kubeconfig)
	if len(out.Created) > 0 {
		fmt.Fprintf(w, "created:    %s\n", strings.Join(out.Created, ", "))
	}
	if out.KubeconfigSaved {
		fmt.Fprintln(w, "saved cluster.bundle_kubeconfig — future applies can omit --kubeconfig")
	}
}

func bundleKubeconfigCmd() *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Show the persisted default kubeconfig venue for bundle applies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out bundleKubeconfigOut
			if offline {
				core, err := offlineCore()
				if err != nil {
					return err
				}
				view, err := core.BundleKubeconfig()
				if err != nil {
					return err
				}
				out = bundleKubeconfigOut{OK: view.OK, Kubeconfig: view.Kubeconfig, Default: view.Default}
			} else {
				session, err := dialMCP(cmd.Context())
				if err != nil {
					return err
				}
				defer session.close()
				if err := session.callTool(cmd.Context(), "outpost_bundle_kubeconfig", map[string]any{}, &out); err != nil {
					return err
				}
			}
			w := cmd.OutOrStdout()
			if out.Kubeconfig != "" {
				fmt.Fprintf(w, "bundle_kubeconfig: %s\n", out.Kubeconfig)
			} else {
				fmt.Fprintf(w, "bundle_kubeconfig: (unset — applies default to %s)\n", out.Default)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "Read the on-disk agent.json instead of the running daemon.")
	return cmd
}
