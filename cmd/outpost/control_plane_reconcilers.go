package main

// Control-plane reconciler startup.
//
// THE DEFECT THIS FILE EXISTS TO FIX.
//
// startControlPlaneReconcilers used to resolve the control-plane admin
// kubeconfig EXACTLY ONCE, synchronously, from inside startControlPlaneTunnel —
// microseconds after the container supervisor goroutine was merely *spawned*.
// The kubeconfig it reads is written by the control-plane container itself
// (`server-entrypoint.sh` sed-rewrites k3s's internal kubeconfig into
// <kubeconfig-dir>/k3s.yaml only after k3s is up), so on any boot where that
// file is not already on disk — the first boot after enabling hosting, a wiped
// kubeconfig dir, a recreated data volume — clientcmd.BuildConfigFromFlags
// returned "no such file or directory", the function logged one Warn and
// RETURNED.
//
// Nothing retried. For the entire lifetime of that daemon process there was
// then no nodecap canary, no nodegc collector, and — the symptom that brought
// this in — no nodeaddr.Reconciler. So no Node ever received the unique
// loopback ExternalIP that the apiserver needs in order to dial a kubelet
// through the authenticated FRP tunnel. The cluster looked perfectly healthy:
// nodes went Ready, pods scheduled, and every `kubectl logs` / `exec` /
// `top nodes` failed. metrics-server, correctly pinned to
// --kubelet-preferred-address-types=ExternalIP, reported exactly what was true:
// "no address matched types [ExternalIP]".
//
// THE FIX IS THE SAME CONTRACT SuperviseServer ALREADY MAKES for the container:
// a cold start only DELAYS the plane, it never DISABLES it until the next
// restart. Resolution now runs under the errgroup and retries with capped
// backoff until the kubeconfig exists and a client can be built, then starts
// the reconcilers and holds them for the life of ctx.
//
// What is deliberately NOT changed: metrics-server stays ExternalIP-only, and
// the ExternalIP that satisfies it is still minted solely by nodeaddr and still
// backed by the authenticated FRP tunnel. This defect was never a reason to
// widen the preferred-address-types list — adding InternalIP/Hostname would
// have made metrics-server dial a kubelet address that is unreachable from the
// control plane's network namespace, trading a loud failure for a silent one
// while dropping the tunnel's authentication boundary.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/nodeaddr"
	"github.com/qiangli/outpost/internal/agent/nodecap"
	"github.com/qiangli/outpost/internal/agent/nodegc"
)

// Backoff bounds for kubeconfig resolution. Vars, not consts, so tests can
// shrink them without sleeping for real seconds.
//
// The ceiling is deliberately low (30s): once the container writes k3s.yaml the
// reconcilers should pick it up promptly, and unlike a network dial there is no
// remote party to be gentle with — this is a stat() against a local path.
var (
	controlPlaneClientFirstBackoff = 2 * time.Second
	controlPlaneClientMaxBackoff   = 30 * time.Second
)

// buildControlPlaneClient turns a kubeconfig path into a clientset. A var so
// the startup tests can substitute a fake clientset and assert that the
// reconcilers actually run, without a kube-apiserver anywhere in sight.
var buildControlPlaneClient = func(path string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// startControlPlaneReconcilers runs the controllers a PEER-HOSTED control
// plane needs and cloudbox would otherwise provide
// (dhnt/docs/dks-control-plane-on-sphere.md).
//
// Gated on this host actually hosting the apiserver. These are cluster-wide
// controllers: running one copy per node would have every node racing to patch
// every other node. Idempotent, so the race is not corrupting — but it is
// pointless API load and makes logs unreadable. When cloudbox hosts the plane
// it runs these itself and outpost must stay out of the way entirely.
//
// Non-blocking: the whole wait-then-run sequence goes on the errgroup, because
// the caller (startControlPlaneTunnel) must return so the rest of boot proceeds.
func startControlPlaneReconcilers(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig) {
	if fc == nil || fc.Cluster == nil || !fc.Cluster.ControlPlaneOn() {
		return
	}
	// The kubeconfig for the plane this host HOSTS — not the one cloudbox
	// issues for the cluster it joins. Without it there is nothing these
	// reconcilers can legitimately act on, so they stay off rather than
	// silently managing the wrong cluster.
	//
	// This is a genuine misconfiguration (nobody recorded a path), unlike a
	// path that merely does not exist YET — so it declines rather than
	// retrying. startControlPlaneTunnel calls rememberControlPlaneKubeconfig
	// before us precisely so this branch is unreachable in the normal flow.
	path := strings.TrimSpace(fc.Cluster.ControlPlaneKubeconfig)
	if path == "" {
		slog.Info("control-plane reconcilers: off (set cluster.control_plane_kubeconfig " +
			"to an admin kubeconfig for the plane this host hosts)")
		return
	}

	g.Go(func() error {
		cs, err := awaitControlPlaneClient(ctx, path, slog.Default())
		if err != nil {
			// Only ever ctx cancellation: awaitControlPlaneClient retries
			// every other failure forever.
			return nil
		}
		runControlPlaneReconcilers(ctx, fc, cs, slog.Default())
		return nil
	})
}

// awaitControlPlaneClient blocks until the control-plane kubeconfig at path can
// be turned into a working client, retrying with capped backoff. It returns an
// error only when ctx is cancelled.
//
// The wait is the entire point. The file is authored by the control-plane
// container after k3s comes up, and this runs at the instant the container
// supervisor is spawned — so "not there yet" is the EXPECTED state on a cold
// boot, not a failure. Treating it as a failure is what silently disabled the
// nodeaddr reconciler for the daemon's whole lifetime.
func awaitControlPlaneClient(ctx context.Context, path string, log *slog.Logger) (kubernetes.Interface, error) {
	if log == nil {
		log = slog.Default()
	}
	backoff := controlPlaneClientFirstBackoff
	waiting := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cs, err := buildControlPlaneClient(path)
		if err == nil {
			if waiting {
				log.Info("control-plane reconcilers: kubeconfig now readable", "kubeconfig", path)
			}
			return cs, nil
		}
		if !waiting {
			waiting = true
			// Info, not Warn: on a cold boot this is the normal sequence, and
			// crying wolf here would train operators to ignore the line. What
			// matters is that the wait is VISIBLE — its absence is what made
			// the original defect undiagnosable from the logs.
			log.Info("control-plane reconcilers: waiting for the control-plane "+
				"container to write its kubeconfig",
				"kubeconfig", path, "err", err, "retry_in", backoff)
		} else {
			log.Debug("control-plane reconcilers: kubeconfig still unavailable",
				"kubeconfig", path, "err", err)
		}
		if !sleepContext(ctx, backoff) {
			return nil, ctx.Err()
		}
		if backoff *= 2; backoff > controlPlaneClientMaxBackoff {
			backoff = controlPlaneClientMaxBackoff
		}
	}
}

// sleepContext waits d, reporting false if ctx was cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runControlPlaneReconcilers starts every control-plane controller against cs
// and blocks until ctx is cancelled and all of them have stopped.
//
// Split out from startControlPlaneReconcilers so a test can drive it with a
// fake clientset and assert on what the reconcilers actually DID — the only
// assertion that fails if a reconciler is dropped from this function.
func runControlPlaneReconcilers(ctx context.Context, fc *conf.FileConfig, cs kubernetes.Interface, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	log.Info("control-plane reconcilers: starting")

	var wg sync.WaitGroup
	run := func(name string, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
		log.Info("control-plane reconcilers: started", "reconciler", name)
	}

	// Runtime capability. Node Ready is a kubelet heartbeat, not proof the
	// runtime can create a sandbox; without this the scheduler places work on
	// nodes that cannot run it.
	//
	// Virtual-kubelet backends have no container runtime of their own to
	// probe, so they are excluded — by RUNTIME LABEL, never by node name.
	cap := &nodecap.Reconciler{
		Client:  cs,
		Log:     log,
		Include: nodecap.DefaultInclude,
	}
	run("runtime-capability-canary", cap.Run)

	// apiserver→kubelet addressing. Without this, nodes go Ready but
	// `kubectl logs`, `exec`, `port-forward` and metrics all fail — the
	// cluster looks healthy while the commands people actually use do not
	// work. This is the reconciler whose absence produced metrics-server's
	// "no address matched types [ExternalIP]".
	//
	// The port is DERIVED from the node name rather than allocated and
	// distributed. Cloudbox allocates per host at pairing because it mediates
	// every join and can hold the pool; a peer-hosted plane has no such party,
	// so both ends compute the same number independently
	// (nodeaddr.KubeletPortForNode). That matters because the tunnel publishes
	// remotePort == localPort — a registry or a config push would each add a
	// way for the two sides to disagree.
	addr := &nodeaddr.Reconciler{
		Client:  cs,
		PortFor: nodeaddr.DerivedKubeletPort,
		Log:     log,
	}
	run("apiserver-kubelet-addressing", addr.Run)

	// Stale-node garbage collection. A peer worker's `cluster leave`
	// deliberately cannot delete its own Node object (it holds only a join
	// token), and every k3s rejoin registers under a new node-id — so NotReady
	// ghosts accumulate on the hosted plane unless the control-plane host reaps
	// them. Bounded, oldest-first, 24 h grace, UID-preconditioned,
	// mass-partition/clock-skew refusals, persisted delete ledger; see
	// internal/agent/nodegc.
	//
	// OUTPOST_NODEGC gates the whole subsystem. Both switches are ALLOWLISTS,
	// never fail-open: only "live" (or a blank/unset default, treated as live)
	// authorizes real deletion, and only "dry-run"/"dryrun"/"dry_run" enters
	// dry run. ANYTHING ELSE — "off", "false", "0", "no", "disabled", or a typo
	// like "of" or "dryrunn" — disables the collector entirely. A mistyped
	// disable must never leak into live deletions, and a mistyped dry-run must
	// never delete for real (DEFECT 4). Env-only for now: adding a four-surface
	// toggle (file key + REST + MCP + CLI) would mean editing
	// admincore/mcpapi/conf, which this change is scoped out of; it is tracked
	// to land as a cluster.node_gc field routed through admincore.SetBuiltins
	// like every other toggle. Until then the off switch for unattended node
	// deletion lives only here — documented in docs/cluster-peer.md.
	gcMode := strings.ToLower(strings.TrimSpace(os.Getenv("OUTPOST_NODEGC")))
	gcLive := gcMode == "" || gcMode == "live" || gcMode == "on" || gcMode == "enabled"
	gcDryRun := gcMode == "dry-run" || gcMode == "dryrun" || gcMode == "dry_run"
	switch {
	case !gcLive && !gcDryRun:
		log.Info("control-plane reconcilers: stale-node garbage collection disabled",
			"OUTPOST_NODEGC", gcMode)
	default:
		// Fail closed on a missing cache dir: without it there is no durable
		// ledger, and the collector refuses to delete without one (DEFECT 3).
		// Surface the resolve error rather than discarding it and running with
		// a nil ledger.
		cacheDir, cacheErr := conf.ResolveCacheDir()
		var gcLedger *nodegc.Ledger
		if cacheErr != nil || cacheDir == "" {
			log.Warn("control-plane reconcilers: no cache dir for nodegc ledger — "+
				"collector will run READ-ONLY (fail closed, no deletions)",
				"err", cacheErr)
		} else {
			gcLedger = nodegc.NewLedger(filepath.Join(cacheDir, "nodegc.log"))
		}
		gc := &nodegc.Collector{
			Client: cs,
			Log:    log,
			DryRun: gcDryRun,
			// SelfHost must match the node HostLabel, which the k3s entrypoint
			// stamps from fc.AgentName — NOT ClusterNodeName(), whose
			// cluster.node_name override diverges from HostLabel and would
			// leave the plane's own agent node unexcluded (DEFECT 5).
			SelfHost: fc.AgentName,
			Ledger:   gcLedger,
		}
		run("stale-node-gc", gc.Run)
		log.Info("control-plane reconcilers: stale-node garbage collection configured",
			"dry_run", gc.DryRun, "ledger", gcLedger.Path())
	}

	wg.Wait()
}
