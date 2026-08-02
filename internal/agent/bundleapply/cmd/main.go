// Command bundleapply is the standalone runner behind
// script/dks-peer-bundle-apply.sh: it applies an app/bundle manifest set
// against a PEER-HOSTED control plane addressed purely by a kubeconfig
// path. Nothing here touches cloudbox — and the venue guard inside the
// bundleapply package (NewDynamicClient) refuses the cloudbox kubeconfig
// even when the script's own check was bypassed.
//
// The same operation is reachable as `outpost bundle apply` (CLI → MCP →
// admincore). This runner stays for the operator script and for hosts
// where no outpost daemon is running.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qiangli/outpost/internal/agent/bundleapply"
)

func main() {
	fs := flag.NewFlagSet("bundleapply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		kubeconfig = fs.String("kubeconfig", "", "path to the target cluster kubeconfig (required; no default cluster)")
		bundle     = fs.String("bundle", "", "path to the bundle (a manifest file or a directory of manifests) (required)")
		timeout    = fs.Duration("timeout", 5*time.Minute, "bounded readiness wait for the whole bundle")
		poll       = fs.Duration("poll", 2*time.Second, "readiness poll interval")
		crdWait    = fs.Duration("crd-timeout", time.Minute, "bounded per-CRD wait for Established + discovery before dependent CRs apply")
		allowZero  = fs.Bool("allow-scale-to-zero", false, "explicit opt-in: a workload with spec.replicas=0 counts as rolled out once drained")
		noRollback = fs.Bool("no-rollback", false, "on failure, leave objects this run created in place (still reported) instead of cleaning them up")
	)
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	// Unknown positional args are a loud failure — no silent tolerance.
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "bundleapply: unexpected arguments: %v\n", fs.Args())
		os.Exit(2)
	}
	if *kubeconfig == "" {
		fmt.Fprintln(os.Stderr, "bundleapply: --kubeconfig is required (there is no default cluster)")
		os.Exit(2)
	}
	if *bundle == "" {
		fmt.Fprintln(os.Stderr, "bundleapply: --bundle is required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *kubeconfig, *bundle, *timeout, *poll, *crdWait, *allowZero, *noRollback); err != nil {
		fmt.Fprintf(os.Stderr, "bundleapply: FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "bundleapply: OK — bundle applied and Ready")
}

func run(ctx context.Context, kubeconfig, bundlePath string, timeout, poll, crdWait time.Duration, allowZero, noRollback bool) error {
	b, err := bundleapply.LoadBundle(bundlePath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "bundleapply: loaded %d object(s) from %s\n", len(b.Objects), bundlePath)

	// NewDynamicClient canonicalizes the path (symlinks resolved) and
	// refuses the cloudbox kubeconfig — the Go-API venue guard.
	client, err := bundleapply.NewDynamicClient(kubeconfig)
	if err != nil {
		return err
	}

	res, err := bundleapply.ApplyBundle(ctx, b, bundleapply.Options{
		Client:           client,
		Timeout:          timeout,
		PollInterval:     poll,
		CRDWaitTimeout:   crdWait,
		AllowScaleToZero: allowZero,
		DisableRollback:  noRollback,
		Log: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "bundleapply: "+format+"\n", args...)
		},
	})
	fmt.Fprintf(os.Stderr, "bundleapply: applied=%d ready=%d created=%d\n", res.Applied, res.Ready, len(res.Created))
	return err
}
