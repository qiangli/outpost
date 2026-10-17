package main

import (
	"context"
	"fmt"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"golang.org/x/sync/errgroup"
)

type peerVisitorTunnel interface {
	Run(context.Context) error
	Close()
}

type peerVisitorFactory func(agent.TunnelConfig, []agent.STCPVisitor) (peerVisitorTunnel, error)

// startPeerVirtualVisitor owns the peer tunnel only when no container owns it.
// Its lifetime is the daemon's, independent of virtual-runner setup retries.
func startPeerVirtualVisitor(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig, newTunnel peerVisitorFactory) error {
	cc := fc.Cluster
	if cc == nil || !cc.JoinsPeerPlane() || cc.HasAgentRuntime() || len(cc.VirtualRuntimes()) == 0 {
		return nil
	}
	if cc.STCPSecret == "" {
		return fmt.Errorf("peer apiserver visitor: missing STCP secret")
	}
	host, port, token, user := fc.JoinTarget()
	tunnel, err := newTunnel(agent.TunnelConfig{
		ServerAddr: host, ServerPort: port, Token: token, User: fc.AgentName, Protocol: "tcp",
	}, []agent.STCPVisitor{{
		Name: "k3s-apiserver-visitor", ServerUser: user, ServerName: "k3s-apiserver",
		Secret: cc.STCPSecret, BindAddr: "127.0.0.1", BindPort: cc.K8sAPIPortOrDefault(),
	}})
	if err != nil {
		return fmt.Errorf("peer apiserver visitor: %w", err)
	}
	g.Go(func() error { defer tunnel.Close(); return tunnel.Run(ctx) })
	return nil
}
