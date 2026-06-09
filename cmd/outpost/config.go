package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// outpost config — settings the SPA/MCP can persist that don't fit any
// of the existing topic-specific subcommands (apps, builtins, outbound,
// cluster, mcp). Right now: networking knobs and the admin-users
// allowlist.
//
// Each `set` flavor uses pointer semantics on the wire: omitted flag =
// leave alone. Pass the literal `<clear>` value to revert a string
// field to its package default at boot.
func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or persist outpost configuration (networking, admin-users allowlist, LAN-discovery)",
	}
	cmd.AddCommand(configShowCmd(), configSetCmd())
	return cmd
}

func configShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the persisted configuration (redacted)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var view admincore.SafeView
			if err := session.readResource(cmd.Context(), "outpost://config", &view); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(view, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Println("Networking")
			row := func(name, val, def string) {
				eff := val
				if eff == "" {
					eff = def + " (default)"
				}
				fmt.Printf("  %-26s  %s\n", name, eff)
			}
			row("local_addr", view.LocalAddr, view.Defaults["local_addr"])
			row("vnc_addr", view.VNCAddr, view.Defaults["vnc_addr"])
			row("admin_addr", view.AdminAddr, view.Defaults["admin_addr"])
			fmt.Println()
			fmt.Println("Admin users (OS-auth allowlist)")
			if len(view.AdminUsers) == 0 {
				fmt.Println("  (empty — any OS user with password = admin)")
			} else {
				for _, e := range view.AdminUsers {
					fmt.Printf("  %s\n", e)
				}
			}
			fmt.Println()
			fmt.Println("LAN peer discovery (Wave 3A)")
			discOnOff := "off"
			if view.DiscoveryEnabled {
				discOnOff = "on"
			}
			fmt.Printf("  %-26s  %s\n", "discovery_enabled", discOnOff)
			row("ssh_listen_addr", view.SSHListenAddr, "(off — matrix tunnel only)")
			row("discovery_http_listen_addr", view.DiscoveryHTTPListenAddr, "(off)")
			fmt.Printf("  %-26s  %s\n", "peer_trust_policy", view.PeerTrustPolicy)
			if view.AssignedHostname != "" {
				fmt.Printf("  %-26s  %s\n", "assigned_hostname", view.AssignedHostname)
			}
			if view.OAuth2Email != "" {
				fmt.Printf("  %-26s  %s\n", "oauth2_email", view.OAuth2Email)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func configSetCmd() *cobra.Command {
	var (
		localAddr, vncAddr, adminAddr string
		adminUsers                    string
		clearAdminUsers               bool

		// Wave 3A discovery + LAN-direct knobs.
		discoveryOn             bool
		discoveryOff            bool
		sshListenAddr           string
		discoveryHTTPListenAddr string
		peerTrustPolicy         string
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Persist networking knobs and / or the admin-users allowlist",
		Long: `Only flags actually supplied are written. Pass the literal value
'<clear>' to revert a string field to its package default at boot.

For the admin-users allowlist, supply --admin-users with a comma-
separated list to apply it, OR pass --clear-admin-users to revert to
the legacy "anyone with OS password is admin" mode.

Examples:
  outpost config set --admin-addr 0.0.0.0:17777          # LAN-bind the admin UI
  outpost config set --vnc-addr 192.168.1.5:5900         # remote VNC daemon
  outpost config set --admin-users alice@x.com,bob@x.com # strict admin set
  outpost config set --clear-admin-users                 # back to OS-trust
  outpost config set --admin-addr '<clear>'              # revert to default
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := admincore.NetworkingParams{}
			set := false
			if cmd.Flags().Changed("local-addr") {
				v := localAddr
				if v == "<clear>" {
					v = ""
				}
				params.LocalAddr = &v
				set = true
			}
			if cmd.Flags().Changed("vnc-addr") {
				v := vncAddr
				if v == "<clear>" {
					v = ""
				}
				params.VNCAddr = &v
				set = true
			}
			if cmd.Flags().Changed("admin-addr") {
				v := adminAddr
				if v == "<clear>" {
					v = ""
				}
				params.AdminAddr = &v
				set = true
			}
			if clearAdminUsers {
				empty := []string{}
				params.AdminUsers = &empty
				set = true
			} else if cmd.Flags().Changed("admin-users") {
				users := splitCSV(adminUsers)
				params.AdminUsers = &users
				set = true
			}
			// Wave 3A discovery + LAN-direct knobs.
			if discoveryOn && discoveryOff {
				return fmt.Errorf("--discovery=on and --no-discovery are mutually exclusive")
			}
			if discoveryOn {
				v := true
				params.DiscoveryEnabled = &v
				set = true
			} else if discoveryOff {
				v := false
				params.DiscoveryEnabled = &v
				set = true
			}
			if cmd.Flags().Changed("ssh-listen-addr") {
				v := sshListenAddr
				if v == "<clear>" {
					v = ""
				}
				params.SSHListenAddr = &v
				set = true
			}
			if cmd.Flags().Changed("discovery-http-listen-addr") {
				v := discoveryHTTPListenAddr
				if v == "<clear>" {
					v = ""
				}
				params.DiscoveryHTTPListenAddr = &v
				set = true
			}
			if cmd.Flags().Changed("peer-trust-policy") {
				v := peerTrustPolicy
				if v == "<clear>" {
					v = ""
				}
				params.PeerTrustPolicy = &v
				set = true
			}
			if !set {
				return fmt.Errorf("nothing to do — pass at least one flag (`outpost config set --help`)")
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			// Translate pointer-strings into the MCP wire shape: empty
			// string = "leave alone" sentinel; <clear> = revert.
			payload := map[string]any{}
			if params.LocalAddr != nil {
				v := *params.LocalAddr
				if v == "" {
					v = "<clear>"
				}
				payload["local_addr"] = v
			}
			if params.VNCAddr != nil {
				v := *params.VNCAddr
				if v == "" {
					v = "<clear>"
				}
				payload["vnc_addr"] = v
			}
			if params.AdminAddr != nil {
				v := *params.AdminAddr
				if v == "" {
					v = "<clear>"
				}
				payload["admin_addr"] = v
			}
			if params.AdminUsers != nil {
				payload["admin_users"] = *params.AdminUsers
				payload["set_admin_users"] = true
			}
			if params.DiscoveryEnabled != nil {
				payload["discovery_enabled"] = *params.DiscoveryEnabled
				payload["set_discovery_enabled"] = true
			}
			if params.SSHListenAddr != nil {
				v := *params.SSHListenAddr
				if v == "" {
					v = "<clear>"
				}
				payload["ssh_listen_addr"] = v
			}
			if params.DiscoveryHTTPListenAddr != nil {
				v := *params.DiscoveryHTTPListenAddr
				if v == "" {
					v = "<clear>"
				}
				payload["discovery_http_listen_addr"] = v
			}
			if params.PeerTrustPolicy != nil {
				v := *params.PeerTrustPolicy
				if v == "" {
					v = "<clear>"
				}
				payload["peer_trust_policy"] = v
			}
			var out struct {
				RestartPending bool `json:"restart_pending"`
			}
			if err := session.callTool(cmd.Context(), "outpost_set_networking", payload, &out); err != nil {
				return err
			}
			if out.RestartPending {
				fmt.Println("Saved. Restarting outpost — poll `outpost status` until configured returns.")
			} else {
				fmt.Println("Saved.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&localAddr, "local-addr", "", "Bind for the matrix-tunnel ingress (default 127.0.0.1:0)")
	cmd.Flags().StringVar(&vncAddr, "vnc-addr", "", "Upstream for the /desktop bridge (default 127.0.0.1:5900)")
	cmd.Flags().StringVar(&adminAddr, "admin-addr", "", "Bind for the admin UI + MCP listener (default 127.0.0.1:17777)")
	cmd.Flags().StringVar(&adminUsers, "admin-users", "", "Comma-separated email allowlist for the OS-auth admin role")
	cmd.Flags().BoolVar(&clearAdminUsers, "clear-admin-users", false, "Revert to the legacy 'anyone with OS password is admin' mode")
	// Wave 3A discovery + LAN-direct.
	cmd.Flags().BoolVar(&discoveryOn, "discovery", false, "Turn LAN peer discovery on (mDNS + HTTP /discover)")
	cmd.Flags().BoolVar(&discoveryOff, "no-discovery", false, "Turn LAN peer discovery off")
	cmd.Flags().StringVar(&sshListenAddr, "ssh-listen-addr", "", "LAN TCP bind for the in-process SSH server (e.g. 0.0.0.0:2222). Pass '<clear>' to disable.")
	cmd.Flags().StringVar(&discoveryHTTPListenAddr, "discovery-http-listen-addr", "", "LAN bind for /api/v1/discover/* (e.g. 0.0.0.0:17778). Pass '<clear>' to disable.")
	cmd.Flags().StringVar(&peerTrustPolicy, "peer-trust-policy", "", "Peer trust policy: same-owner | same-cloudbox | tofu-allow")
	return cmd
}

func splitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
