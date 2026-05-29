package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/userkube"
)

// kubectlCmd is `outpost kubectl <args…>` — a thin pass-through to the
// system kubectl with KUBECONFIG pointing at a freshly-minted per-user
// kubeconfig in the outpost cache directory. The point is to spare
// the operator from juggling three different kubeconfigs (node-scoped
// SA, user-scoped SA, empty) and from manually re-running
// `outpost cluster userkubeconfig` every hour when the token expires.
//
// Token refresh policy:
//
//   - Re-fetch when the cached file is missing, unreadable, or
//     older than kubeconfigStaleAfter (50 min, half the cloudbox-
//     side 1 h token TTL plus a buffer).
//   - Re-fetch on demand via `outpost kubectl --refresh ...`.
//
// The cached file lives at <cacheDir>/cluster-kubeconfig.yaml with
// 0600 perms (carries a bearer token).
//
// Why default to the user kubeconfig (not the node one):
//
//	The node-scoped kubeconfig (`outpost cluster kubeconfig`) is for
//	the outpost daemon itself — its RBAC is whatever the
//	cloudbox-side `outpost-nodes:<host>` ServiceAccount carries,
//	which is intentionally narrow (no deployments/list, etc).
//	Operator-mode `kubectl get deploy` / `kubectl apply -f` need
//	the per-user SA, which is the default here.
func kubectlCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:                "kubectl [args...]",
		Short:              "Run kubectl against this cloudbox cluster (auto-fetches a per-user kubeconfig)",
		DisableFlagParsing: true, // all flags are kubectl's; the only outpost flag (--refresh) is consumed before kubectl sees args
		RunE: func(cmd *cobra.Command, args []string) error {
			args, refresh = splitOutpostFlags(args)

			path, err := ensureUserKubeconfig(cmd.Context(), refresh)
			if err != nil {
				return err
			}

			kc, err := exec.LookPath("kubectl")
			if err != nil {
				return fmt.Errorf("kubectl not found on PATH: %w", err)
			}

			// Exec — replaces this process so Ctrl-C and signal
			// handling reach kubectl directly. On Windows this
			// degrades to a regular Run() since syscall.Exec
			// isn't available; we fall back below.
			if err := execKubectl(kc, args, path); err != nil {
				return err
			}
			return nil
		},
	}
	return cmd
}

const (
	cachedUserKubeconfigName = "cluster-kubeconfig.yaml"
	kubeconfigStaleAfter     = 50 * time.Minute
)

// splitOutpostFlags peels off the wrapper's own flags (currently only
// --refresh) before handing the rest to kubectl. Honors the standard
// `--` separator: anything after it goes through unmodified.
func splitOutpostFlags(args []string) (kubectlArgs []string, refresh bool) {
	out := make([]string, 0, len(args))
	sawSeparator := false
	for _, a := range args {
		if sawSeparator {
			out = append(out, a)
			continue
		}
		if a == "--" {
			sawSeparator = true
			out = append(out, a)
			continue
		}
		if a == "--refresh" {
			refresh = true
			continue
		}
		out = append(out, a)
	}
	return out, refresh
}

// ensureUserKubeconfig returns the path to a fresh-enough cached
// per-user kubeconfig, re-fetching from cloudbox when needed.
func ensureUserKubeconfig(ctx context.Context, refresh bool) (string, error) {
	cacheDir, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("ensure cache dir %s: %w", cacheDir, err)
	}
	path := filepath.Join(cacheDir, cachedUserKubeconfigName)

	if !refresh && kubeconfigFresh(path) {
		return path, nil
	}

	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return "", err
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		return "", fmt.Errorf("load %s: %w", cfgPath, err)
	}
	if fc == nil || fc.AccessToken == "" {
		return "", errors.New("no access_token saved — run `outpost register` first, then `outpost builtins set --cluster=on`")
	}
	cloudboxBase := cloudboxHTTPBase(fc)
	if cloudboxBase == "" {
		return "", errors.New("no cloudbox URL in saved config (server_addr / protocol missing)")
	}

	yaml, err := userkube.FetchUserKubeconfigYAML(ctx, cloudboxBase, fc.AccessToken)
	if err != nil {
		// Stale cache is still preferable to a hard fail when
		// cloudbox is briefly unreachable — kubectl will surface
		// any auth error itself if the token expired.
		if !refresh && fileExists(path) {
			fmt.Fprintf(os.Stderr, "warning: failed to refresh kubeconfig (%v); using cached copy\n", err)
			return path, nil
		}
		return "", fmt.Errorf("fetch user kubeconfig: %w", err)
	}
	if err := userkube.WriteStandalone(yaml, path); err != nil {
		return "", err
	}
	return path, nil
}

func kubeconfigFresh(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(st.ModTime()) < kubeconfigStaleAfter
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// execKubectl replaces this process with kubectl (Unix) or runs it as
// a child + propagates exit code (Windows). KUBECONFIG is set in the
// child env only — the operator's existing $KUBECONFIG and
// ~/.kube/config stay untouched.
func execKubectl(kc string, args []string, kubeconfigPath string) error {
	// Override KUBECONFIG for the child. Build env explicitly so we
	// drop any existing KUBECONFIG without mutating the parent.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "KUBECONFIG="+kubeconfigPath)

	argv := append([]string{kc}, args...)

	// syscall.Exec swaps the process image, so kubectl's exit code
	// becomes outpost's exit code without us having to forward it.
	// On platforms where syscall.Exec is a no-op stub (none of our
	// supported ones), the function returns ENOSYS — fall back to
	// a child invocation below.
	if err := syscall.Exec(kc, argv, env); err != nil && err != syscall.ENOSYS {
		// Real exec failure — surface it.
		return fmt.Errorf("exec kubectl: %w", err)
	}

	// Fallback for the (theoretical) no-exec platform.
	c := exec.Command(kc, args...)
	c.Env = env
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
