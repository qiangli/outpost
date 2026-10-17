package main

import (
	"context"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"golang.org/x/sync/errgroup"
)

type fakePeerVisitor struct{ closed bool }

func (f *fakePeerVisitor) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (f *fakePeerVisitor) Close()                        { f.closed = true }

func TestPeerVirtualVisitor(t *testing.T) {
	for _, physical := range []bool{false, true} {
		t.Run(map[bool]string{false: "virtual-only", true: "container"}[physical], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := new(errgroup.Group)
			fc := &conf.FileConfig{AgentName: "worker", Cluster: &conf.ClusterConfig{
				JoinEndpoint: "peer.example:7001", JoinToken: "session-secret", STCPSecret: "visitor-secret", K8sAPIPort: 7443,
				Runtimes: conf.ClusterRuntimes{Agent: physical, Virtual: []string{conf.ClusterRuntimeVKNative}},
			}}
			fake := new(fakePeerVisitor)
			called := false
			err := startPeerVirtualVisitor(ctx, g, fc, func(tc agent.TunnelConfig, vs []agent.STCPVisitor) (peerVisitorTunnel, error) {
				called = true
				if tc.ServerAddr != "peer.example" || tc.ServerPort != 7001 || tc.Token != "session-secret" || tc.Protocol != "tcp" || tc.User != "worker" {
					t.Fatalf("wrong tunnel config: %+v", tc)
				}
				if len(vs) != 1 || vs[0].ServerUser != conf.ControlPlanePublisherUser || vs[0].ServerName != "k3s-apiserver" || vs[0].Secret != "visitor-secret" || vs[0].BindAddr != "127.0.0.1" || vs[0].BindPort != 7443 {
					t.Fatalf("wrong visitors: %+v", vs)
				}
				return fake, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := g.Wait(); err != nil {
				t.Fatal(err)
			}
			if called == physical || fake.closed == physical {
				t.Fatalf("called=%v closed=%v physical=%v", called, fake.closed, physical)
			}
		})
	}
}
