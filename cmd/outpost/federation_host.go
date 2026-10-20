package main

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/federation"
	"github.com/qiangli/outpost/internal/agent/fleetreg"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

// federationHost is the running host reconciler, nil until federation is on and
// the plane kubeconfig exists. admincore reads it through federationHostState.
var federationHost atomic.Pointer[federation.HostReconciler]

// federationMember is the running member reconciler (a host is one or the
// other, never both).
var federationMember atomic.Pointer[federation.MemberReconciler]

func federationHostState() federation.HostState {
	if r := federationHost.Load(); r != nil {
		return r.State()
	}
	if r := federationMember.Load(); r != nil {
		return r.State()
	}
	return federation.HostState{}
}

// federationFactsFromConfig is the production reader behind federationFacts.
// It reports nothing until the CA hash is known: the hash IS the identity, and
// NewFederationInfo returns nil without it.
func federationFactsFromConfig(cc *conf.ClusterConfig) (caHash, role, clusterName, syncMode string) {
	if cc == nil || !cc.FederationOn() {
		return "", "", "", ""
	}
	f := cc.Federation
	if strings.TrimSpace(f.CAHash) == "" {
		return "", "", "", ""
	}
	role = fleetreg.FederationRoleMember
	if f.Host {
		role = fleetreg.FederationRoleHost
	}
	return f.CAHash, role, f.ClusterName, fleetreg.FederationSyncPull
}

// startFederationHost runs the host reconciler for the daemon's life: install
// the karmada built-in, materialise the kubeconfig, learn the CA hash and
// register this host's own plane. Like the other control-plane reconcilers it
// waits (forever) for the plane kubeconfig rather than failing on a cold start.
func startFederationHost(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig, core *admincore.Server) {
	if fc == nil || fc.Cluster == nil || !fc.Cluster.ControlPlaneOn() || !fc.Cluster.Federation.HostOn() {
		return
	}
	planePath := strings.TrimSpace(fc.Cluster.ControlPlaneKubeconfig)
	if planePath == "" {
		slog.Info("federation: host reconciler off (no control-plane kubeconfig recorded)")
		return
	}
	if err := federation.CheckOS(); err != nil {
		slog.Warn("federation: disabled", "err", err)
		return
	}
	f := fc.Cluster.Federation
	g.Go(func() error {
		cs, err := awaitControlPlaneClient(ctx, planePath, slog.Default())
		if err != nil {
			return nil
		}
		restCfg, err := clientcmd.BuildConfigFromFlags("", planePath)
		if err != nil {
			slog.Warn("federation: plane kubeconfig unusable", "err", err)
			return nil
		}
		dyn, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			slog.Warn("federation: dynamic client", "err", err)
			return nil
		}
		name := f.ClusterName
		if name == "" {
			name = fc.AgentName
		}
		r := &federation.HostReconciler{
			Plane: federation.KubePlane{Client: cs, Dyn: dyn},
			Install: func(ctx context.Context) error {
				_, err := core.BuiltinInstall(ctx, admincore.BuiltinInstallParams{
					Name: "karmada", Kubeconfig: planePath, NoRollback: true, TimeoutSeconds: 900,
				})
				return err
			},
			KubeconfigPath:  f.KubeconfigPath(),
			Port:            f.Port(),
			ClusterName:     name,
			PlaneKubeconfig: planePath,
			Runner:          federation.ExecRunner{},
			Members: func(_ context.Context, kubeconfig string) ([]federation.Member, error) {
				k, err := federation.NewKubeKarmada(kubeconfig)
				if err != nil {
					return nil, err
				}
				return k.Members(ctx)
			},
			RecordHash:   core.RecordFederationCAHash,
			PostRegister: func(ctx context.Context) error { return federation.RewriteMemberAgentKubeconfig(ctx, cs) },
			Log:          slog.Default(),
		}
		federationHost.Store(r)
		r.Run(ctx)
		return nil
	})
}

// applyFederationVisitor threads a member's federation visitor into the agent
// runtime: the third frpc session to the federation host's frps, bound on
// 0.0.0.0:<port> in the container and published on host 127.0.0.1:<port>.
// Members only; a malformed endpoint leaves the visitor off (logged) rather
// than blocking the node.
func applyFederationVisitor(cc *conf.ClusterConfig, opts *runtime.Options) {
	if cc == nil || !cc.Federation.MemberOn() || federation.CheckOS() != nil {
		return
	}
	f := cc.Federation
	host, portStr, err := net.SplitHostPort(f.Endpoint)
	port, perr := strconv.Atoi(portStr)
	if err != nil || perr != nil || host == "" {
		slog.Warn("federation: member endpoint unusable; visitor off", "endpoint", f.Endpoint)
		return
	}
	opts.FederationHost = host
	opts.FederationPort = port
	opts.FederationProtocol = "tcp" // a peer frps is plain TCP, like a peer-plane join
	opts.FederationToken = f.Token
	opts.FederationSecret = f.STCPSecret
	opts.FederationBindPort = f.Port()
}

// startFederationMember runs the member reconciler for the daemon's life: keep
// the mirror Service/Endpoints at this node's InternalIP, finish a pending
// join once the visitor is bound, keep exceptions 1 and 2 applied.
func startFederationMember(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig) {
	if fc == nil || fc.Cluster == nil || !fc.Cluster.Federation.MemberOn() {
		return
	}
	if err := federation.CheckOS(); err != nil {
		slog.Warn("federation: disabled", "err", err)
		return
	}
	planePath := strings.TrimSpace(fc.Cluster.ControlPlaneKubeconfig)
	if !fc.Cluster.ControlPlaneOn() || planePath == "" {
		slog.Warn("federation: member reconciler off (no hosted control plane / kubeconfig)")
		return
	}
	if !fc.ClusterOn() || !fc.Cluster.HasAgentRuntime() {
		slog.Warn("federation: member has no agent runtime; the visitor lives in it, so registration cannot proceed")
	}
	f := fc.Cluster.Federation
	pending, err := federation.DefaultPendingJoinPath()
	if err != nil {
		slog.Warn("federation: pending-join path", "err", err)
		return
	}
	g.Go(func() error {
		cs, err := awaitControlPlaneClient(ctx, planePath, slog.Default())
		if err != nil {
			return nil
		}
		name := f.ClusterName
		if name == "" {
			name = fc.AgentName
		}
		r := &federation.MemberReconciler{
			Client:          cs,
			NodeName:        fc.ClusterNodeName(),
			ClusterName:     name,
			PlaneKubeconfig: planePath,
			CAHash:          f.CAHash,
			Port:            f.Port(),
			PendingPath:     pending,
			Runner:          federation.ExecRunner{},
			Log:             slog.Default(),
		}
		federationMember.Store(r)
		r.Run(ctx)
		return nil
	})
}

// federationLeave is admincore's FederationLeave seam: unregister through the
// still-running visitor, then drop the mirror. A join that never registered
// has nothing to unregister.
func federationLeave(ctx context.Context, clusterName, planeKubeconfig string, pending bool) error {
	cs, err := buildControlPlaneClient(planeKubeconfig)
	if err != nil {
		return err
	}
	if pending {
		return federation.RemoveMirror(ctx, cs)
	}
	return federation.LeaveMember(ctx, federation.ExecRunner{}, cs, clusterName, planeKubeconfig)
}
