package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/mesh"
)

// meshFwdAdapter adapts a *mesh.Forwarder to admincore.MeshForwardOps so the
// daemon threads it into admincore.Deps without admincore importing the mesh
// package.
type meshFwdAdapter struct{ f *mesh.Forwarder }

func (a meshFwdAdapter) Expose(service, addr string) error { a.f.Expose(service, addr); return nil }
func (a meshFwdAdapter) Unexpose(service string) error     { a.f.Unexpose(service); return nil }

func (a meshFwdAdapter) Listen(peerID, service, localAddr string) (string, error) {
	ln, err := a.f.Listen(localAddr, peerID, service)
	if err != nil {
		return "", err
	}
	return ln.Addr().String(), nil
}

func (a meshFwdAdapter) CloseListen(addr string) error { return a.f.CloseListen(addr) }

func (a meshFwdAdapter) Forwards() admincore.MeshForwardView {
	snap := a.f.Snapshot()
	v := admincore.MeshForwardView{Exposed: snap.Exposed}
	for _, l := range snap.Listeners {
		v.Listeners = append(v.Listeners, admincore.MeshListenerView{Addr: l.Addr, PeerID: l.PeerID, Service: l.Service})
	}
	return v
}

func meshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "Inspect + drive the libp2p mesh data plane (peer-to-peer transport)",
		Long: `The mesh carries a loopback TCP service peer-to-peer over a direct,
hole-punched link (cloudbox brokers the introduction; the bytes go
peer-to-peer). On the worker, Expose a local service; on the leader, Listen for
it and point a client (e.g. llama-server --rpc) at the printed local address.`,
	}
	cmd.AddCommand(meshStatusCmd(), meshExposeCmd(), meshUnexposeCmd(), meshListenCmd(), meshUnlistenCmd())
	return cmd
}

func meshStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the mesh peer id, connected peers, and forwarder state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out struct {
				Status   *admincore.MeshStatusView `json:"status"`
				Forwards admincore.MeshForwardView `json:"forwards"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mesh_status", struct{}{}, &out); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if out.Status == nil {
				fmt.Println("mesh host not up")
			} else {
				fmt.Printf("peer_id:         %s\n", out.Status.PeerID)
				fmt.Printf("connected_peers: %d\n", out.Status.ConnectedPeers)
				for _, a := range out.Status.ListenAddrs {
					fmt.Printf("  listen %s\n", a)
				}
			}
			for svc, addr := range out.Forwards.Exposed {
				fmt.Printf("exposed: %s -> %s\n", svc, addr)
			}
			for _, l := range out.Forwards.Listeners {
				fmt.Printf("listener: %s -> peer %s service %s\n", l.Addr, l.PeerID, l.Service)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}

func meshExposeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "expose <service> <loopback-addr>",
		Short: "Expose a local loopback service over the mesh (worker side)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				OK bool `json:"ok"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mesh_expose",
				map[string]string{"service": args[0], "addr": args[1]}, &out); err != nil {
				return err
			}
			fmt.Printf("exposed %s -> %s over the mesh\n", args[0], args[1])
			return nil
		},
	}
}

func meshUnexposeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unexpose <service>",
		Short: "Stop exposing a mesh service (worker side)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				OK bool `json:"ok"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mesh_unexpose",
				map[string]string{"service": args[0]}, &out); err != nil {
				return err
			}
			fmt.Printf("unexposed %s\n", args[0])
			return nil
		},
	}
}

func meshListenCmd() *cobra.Command {
	var local string
	cmd := &cobra.Command{
		Use:   "listen <peer-id> <service>",
		Short: "Open a local listener forwarding to (peer, service) over the mesh (leader side)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Addr string `json:"addr"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mesh_listen",
				map[string]string{"peer_id": args[0], "service": args[1], "local_addr": local}, &out); err != nil {
				return err
			}
			fmt.Println(out.Addr)
			return nil
		},
	}
	cmd.Flags().StringVar(&local, "local", "", "Local listen address (default 127.0.0.1:0 = ephemeral)")
	return cmd
}

func meshUnlistenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlisten <addr>",
		Short: "Close a mesh forward listener by its bound local address (leader side)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				OK bool `json:"ok"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mesh_close_listen",
				map[string]string{"addr": args[0]}, &out); err != nil {
				return err
			}
			fmt.Printf("closed listener %s\n", args[0])
			return nil
		},
	}
}

func runMeshTool(ctx context.Context, name string, args, out any) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	return session.callTool(ctx, name, args, out)
}
