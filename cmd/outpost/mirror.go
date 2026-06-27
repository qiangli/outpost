package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// mirrorCmd manages mobility-aware continuous directory mirrors (MCP clients).
func mirrorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Mobility-aware continuous directory mirror to a peer over the mesh",
		Long: `Keep a peer's replica in near-real-time sync with a local directory, but only
while the peer is reachable (and same-LAN with --lan-only): it pauses when the
mirrored pair goes remote and resumes — catching up via a full sync — when local
again. Distinct from 'backup' (encrypted scheduled snapshots to cloudbox).

The replica peer exposes its receive dir as a mesh service, e.g.:
  replica$  bashy rclone serve webdav /replica --addr 127.0.0.1:8080
  replica$  outpost mesh service add webdav 127.0.0.1:8080
then this node:  outpost mirror add /data webdav --lan-only`,
	}

	var lanOnly bool
	add := &cobra.Command{
		Use:   "add <source-dir> <mesh-service>",
		Short: "Mirror <source-dir> to the peer exposing <mesh-service>",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				OK bool `json:"ok"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mirror_set",
				map[string]any{"source": args[0], "service": args[1], "lan_only": lanOnly}, &out); err != nil {
				return err
			}
			fmt.Printf("mirroring %s -> peer service %q (lan_only=%v); restarting to apply\n", args[0], args[1], lanOnly)
			return nil
		},
	}
	add.Flags().BoolVar(&lanOnly, "lan-only", false, "mirror only while the peer is on the same LAN (pause over WAN)")

	rm := &cobra.Command{
		Use:   "rm <source-dir>",
		Short: "Remove a mirror job by its source directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				OK bool `json:"ok"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mirror_rm",
				map[string]any{"source": args[0]}, &out); err != nil {
				return err
			}
			fmt.Printf("removed mirror job %s; restarting to apply\n", args[0])
			return nil
		},
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List mirror jobs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out struct {
				Enabled bool `json:"enabled"`
				Jobs    []struct {
					Source  string `json:"source"`
					Service string `json:"service"`
					LANOnly bool   `json:"lan_only"`
				} `json:"jobs"`
			}
			if err := runMeshTool(cmd.Context(), "outpost_mirror_list", struct{}{}, &out); err != nil {
				return err
			}
			fmt.Printf("enabled: %v\n", out.Enabled)
			for _, j := range out.Jobs {
				fmt.Printf("%s -> service %q\tlan_only=%v\n", j.Source, j.Service, j.LANOnly)
			}
			return nil
		},
	}

	cmd.AddCommand(add, rm, ls)
	return cmd
}
