package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// `outpost ssh authorize ...` — manage the public keys the `outpost.sshd`
// service unit accepts for LAN logins.
//
// The keys live in the unit's authorized_keys file under the outpost
// config dir (OpenSSH format), and take effect immediately — the server
// re-reads the file on every authentication. Keys authenticate the daemon
// OS account only; the OS password stays available alongside.
//
// Surface:
//
//	outpost ssh authorize add <pubkey|->   # append a key (or read it from stdin with -)
//	outpost ssh authorize rm <fingerprint> # drop a key by its SHA256 fingerprint
//	outpost ssh authorize ls                # fingerprints only
func sshAuthorizeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "authorize",
		Short: "Manage the public keys the LAN sshd unit accepts (add|rm|ls)",
		Long: `Manage the outpost.sshd unit's authorized_keys file.

  outpost ssh authorize add "ssh-ed25519 AAAA... laptop"
  ssh-keygen -y -f ~/.ssh/id_ed25519 | outpost ssh authorize add -
  outpost ssh authorize ls
  outpost ssh authorize rm SHA256:...

add is idempotent (re-adding a present key returns its fingerprint), rm
is idempotent (an absent fingerprint reports removed:false), and both
take effect immediately with no daemon restart. ls prints SHA256
fingerprints only — key material never leaves the host.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		sshAuthorizeAddCmd(),
		sshAuthorizeRmCmd(),
		sshAuthorizeLsCmd(),
	)
	return cmd
}

func sshAuthorizeAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <pubkey|->",
		Short: "Authorize a public key for LAN SSH logins",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			if key == "-" {
				raw, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				key = strings.TrimSpace(string(raw))
			}
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("no public key given")
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Fingerprint string `json:"fingerprint"`
			}
			if err := session.callTool(cmd.Context(), "outpost_ssh_authorize_add", map[string]any{
				"key": key,
			}, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", out.Fingerprint)
			return nil
		},
	}
}

func sshAuthorizeRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <fingerprint>",
		Short: "Remove a public key from the LAN SSH authorized_keys file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Removed bool `json:"removed"`
			}
			if err := session.callTool(cmd.Context(), "outpost_ssh_authorize_remove", map[string]any{
				"fingerprint": args[0],
			}, &out); err != nil {
				return err
			}
			if out.Removed {
				fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", strings.TrimSpace(args[0]))
			} else {
				fmt.Fprintf(os.Stderr, "No authorized key matches %s\n", strings.TrimSpace(args[0]))
			}
			return nil
		},
	}
}

func sshAuthorizeLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List fingerprints of keys authorized for LAN SSH logins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Fingerprints []string `json:"fingerprints"`
			}
			if err := session.callTool(cmd.Context(), "outpost_ssh_authorize_list", map[string]any{}, &out); err != nil {
				return err
			}
			for _, fp := range out.Fingerprints {
				fmt.Fprintln(cmd.OutOrStdout(), fp)
			}
			return nil
		},
	}
}
