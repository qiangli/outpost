// `outpost remote {login,logout,list}` caches the bearer token + admin
// endpoint for outposts on other machines so the CLI can target them
// with `outpost --remote <name> apps stop foo` instead of piping
// $OUTPOST_HOST / $OUTPOST_MCP_TOKEN on every invocation.
//
// Cache layout (mode 0600, same OS user only):
//
//	~/.config/outpost/remotes/<name>.json
//	  { "addr": "host.local:17777", "token": "<bearer>" }
//
// Names are arbitrary aliases — typically the LAN hostname, but
// nothing here interprets them. Token is the value of the remote
// outpost's FileConfig.MCPBearerToken (printed by `outpost mcp
// endpoint` on that machine).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// remoteEntry is the on-disk shape of a cached remote.
type remoteEntry struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func remoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage cached credentials for other outposts (LAN deploy targets)",
	}
	cmd.AddCommand(remoteLoginCmd(), remoteLogoutCmd(), remoteListCmd())
	return cmd
}

func remoteLoginCmd() *cobra.Command {
	var (
		addrFlag  string
		tokenFlag string
		fromStdin bool
	)
	cmd := &cobra.Command{
		Use:   "login <name>",
		Short: "Cache credentials for a remote outpost (paste bearer when prompted)",
		Long: `Stores the admin endpoint + MCP bearer for a remote outpost under
~/.config/outpost/remotes/<name>.json (mode 0600). Subsequent CLI
calls can target the remote with --remote <name> or $OUTPOST_REMOTE.

On the remote machine, run "outpost mcp endpoint" to print the bearer.
The admin listener must be bound to a LAN-reachable address (default
binds loopback only — set admin_addr to 0.0.0.0:17777 or a LAN IP).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validRemoteName(name); err != nil {
				return err
			}
			addr := strings.TrimSpace(addrFlag)
			if addr == "" {
				suggested := name + ":17777"
				v, err := promptDefault(bufio.NewReader(os.Stdin), "Admin endpoint", suggested)
				if err != nil {
					return err
				}
				addr = strings.TrimSpace(v)
			}
			tok := strings.TrimSpace(tokenFlag)
			if tok == "" {
				if fromStdin {
					b, err := os.ReadFile("/dev/stdin")
					if err != nil {
						return fmt.Errorf("read token from stdin: %w", err)
					}
					tok = strings.TrimSpace(string(b))
				} else {
					v, err := promptRequired(bufio.NewReader(os.Stdin), "MCP bearer token")
					if err != nil {
						return err
					}
					tok = strings.TrimSpace(v)
				}
			}
			if addr == "" || tok == "" {
				return fmt.Errorf("both endpoint and token are required")
			}
			if err := saveRemoteEntry(name, remoteEntry{Addr: addr, Token: tok}); err != nil {
				return err
			}
			fmt.Printf("Saved remote %q (%s)\n", name, addr)
			return nil
		},
	}
	cmd.Flags().StringVar(&addrFlag, "addr", "", "Admin endpoint (host:port or http://host:port). Prompted when omitted.")
	cmd.Flags().StringVar(&tokenFlag, "token", "", "MCP bearer token. Prompted when omitted (or piped via --stdin).")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read the bearer from stdin (for piped passwords / scripts)")
	return cmd
}

func remoteLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout <name>",
		Short: "Forget cached credentials for a remote outpost",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := remoteEntryPath(args[0])
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				if os.IsNotExist(err) {
					fmt.Printf("No cached remote named %q.\n", args[0])
					return nil
				}
				return err
			}
			fmt.Printf("Removed remote %q.\n", args[0])
			return nil
		},
	}
}

func remoteListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached remotes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := remotesDir()
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					if jsonOut {
						fmt.Println("[]")
					} else {
						fmt.Println("No cached remotes.")
					}
					return nil
				}
				return err
			}
			type row struct {
				Name string `json:"name"`
				Addr string `json:"addr"`
			}
			rows := []row{}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				name := strings.TrimSuffix(e.Name(), ".json")
				re, err := loadRemoteEntry(name)
				if err != nil {
					continue
				}
				rows = append(rows, row{Name: name, Addr: re.Addr})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
			if jsonOut {
				b, _ := json.MarshalIndent(rows, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(rows) == 0 {
				fmt.Println("No cached remotes.")
				return nil
			}
			fmt.Printf("%-20s  %s\n", "NAME", "ADDR")
			for _, r := range rows {
				fmt.Printf("%-20s  %s\n", r.Name, r.Addr)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

// loadRemoteEntry reads ~/.config/outpost/remotes/<name>.json. Called
// by mcpclient.resolveMCPTarget when --remote is set.
func loadRemoteEntry(name string) (*remoteEntry, error) {
	if err := validRemoteName(name); err != nil {
		return nil, err
	}
	path, err := remoteEntryPath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no cached remote named %q — run `outpost remote login %s`", name, name)
		}
		return nil, err
	}
	var re remoteEntry
	if err := json.Unmarshal(b, &re); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &re, nil
}

func saveRemoteEntry(name string, re remoteEntry) error {
	if err := validRemoteName(name); err != nil {
		return err
	}
	dir, err := remotesDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(re, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".json")
	// O_TRUNC so a re-login overwrites the previous bearer cleanly.
	return os.WriteFile(path, b, 0o600)
}

func remotesDir() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "outpost", "remotes"), nil
}

func remoteEntryPath(name string) (string, error) {
	dir, err := remotesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// validRemoteName guards against path traversal — names land directly
// in a filesystem path.
func validRemoteName(name string) error {
	if name == "" {
		return fmt.Errorf("remote name is required")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("invalid remote name %q (allowed: letters, digits, -, _, .)", name)
		}
	}
	return nil
}
