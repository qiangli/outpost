package main

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/federation"
	"github.com/qiangli/outpost/internal/agent/fleetreg"
)

// federationHost is the running host reconciler, nil until federation is on and
// the plane kubeconfig exists. admincore reads it through federationHostState.
var federationHost atomic.Pointer[federation.HostReconciler]

func federationHostState() federation.HostState {
	if r := federationHost.Load(); r != nil {
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
