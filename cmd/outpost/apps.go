package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// outpost apps … — CLI mirror of the admin UI's Inbound > Custom Apps
// panel. Each subcommand connects to the running daemon's /mcp/
// endpoint and calls one outpost_*_app tool.
func appsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "Manage custom apps (CLI mirror of the admin UI Inbound tab)",
	}
	cmd.AddCommand(
		appsListCmd(),
		appsAddCmd(),
		appsRmCmd(),
		appsStopCmd(),
		appsStartCmd(),
		appsRotateTokenCmd(),
		appsSecretCmd(),
		appsRotateSecretCmd(),
		appsSuggestCmd(),
	)
	return cmd
}

// appsStopCmd / appsStartCmd flip an app's Enabled flag without re-
// supplying the rest of its config. The proxy gate flips immediately
// (cloudbox-side tile starts 503'ing on stop), but the upstream
// container/process is untouched — operators stop those out-of-band
// (e.g. `podman stop <name>` or `systemctl stop …`).
func appsStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Disable an app's proxy gate (upstream untouched)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetAppEnabled(cmd.Context(), args[0], false)
		},
	}
}

func appsStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Enable an app's proxy gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetAppEnabled(cmd.Context(), args[0], true)
		},
	}
}

func runSetAppEnabled(ctx context.Context, name string, enabled bool) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out struct {
		OK  bool           `json:"ok"`
		App conf.AppConfig `json:"app"`
	}
	if err := session.callTool(ctx, "outpost_set_app_enabled", map[string]any{
		"name":    name,
		"enabled": enabled,
	}, &out); err != nil {
		return err
	}
	verb := "started"
	if !enabled {
		verb = "stopped"
	}
	fmt.Printf("%s app %q (enabled=%t)\n", verb, out.App.Name, out.App.Enabled)
	return nil
}

func appsListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list [host]",
		Short: "List registered custom apps (local daemon, or a paired remote host)",
		Long: `outpost apps list           # local daemon's app registry
outpost apps list <host>    # apps on a paired remote host (via cloudbox)

Without a positional, lists the local outpost's app registry via the
running daemon's MCP endpoint. With <host>, fetches the remote host's
catalog from cloudbox's /api/v1/hosts (using the persisted access
token's ssh:read scope) and prints the app slugs. The remote view
shows only what cloudbox last polled from the host's GET /apps — it's
the same data the SPA renders for that host's tiles.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runAppsListRemote(cmd.Context(), args[0], jsonOut)
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var view struct {
				Apps []conf.AppConfig `json:"apps"`
			}
			if err := session.readResource(cmd.Context(), "outpost://apps", &view); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(view, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(view.Apps) == 0 {
				fmt.Println("No apps registered.")
				return nil
			}
			fmt.Printf("%-20s  %-8s  %-7s  %-30s  %s\n", "NAME", "SCHEME", "ENABLED", "TARGET", "FLAGS")
			for _, a := range view.Apps {
				flags := []string{}
				if a.RequireLogin {
					flags = append(flags, "login")
				}
				if a.TrustCloudIdentity {
					flags = append(flags, "sso")
				}
				if a.IndexPath != "" {
					flags = append(flags, "index="+a.IndexPath)
				}
				if len(a.LANOnlyPaths) > 0 {
					flags = append(flags, "lan-only="+strings.Join(a.LANOnlyPaths, ","))
				}
				target := a.Socket
				if target == "" {
					target = fmt.Sprintf("%s:%d", a.Host, a.Port)
				}
				fmt.Printf("%-20s  %-8s  %-7t  %-30s  %s\n",
					a.Name, a.Scheme, a.Enabled, target, strings.Join(flags, " "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

// remoteAppEntry mirrors cloudbox's V1HostAppEntry — the subset of
// fields the CLI surfaces for a remote host's app catalog. Keeping
// this type local (rather than importing the cloudbox handler types)
// preserves the OSS / proprietary boundary: outpost ships in the
// public github.com/qiangli/outpost repo, cloudbox is internal.
type remoteAppEntry struct {
	Name         string `json:"name"`
	Scheme       string `json:"scheme,omitempty"`
	RequireLogin bool   `json:"require_login"`
	IndexPath    string `json:"index_path,omitempty"`
}

type remoteHostView struct {
	Host   string           `json:"host"`
	Online bool             `json:"online"`
	Shared bool             `json:"shared,omitempty"`
	Apps   []remoteAppEntry `json:"apps"`
}

func runAppsListRemote(ctx context.Context, host string, jsonOut bool) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return errors.New("apps list: empty host")
	}
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("locate config: %w", err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if fc == nil || fc.ServerAddr == "" {
		return errors.New("local outpost is not paired with cloudbox — run `outpost register` first")
	}
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		return errors.New("no cloudbox bearer cached — re-pair with `outpost register`")
	}

	view, err := fetchRemoteHostApps(ctx, fc.ServerAddr, fc.ServerPort, fc.Protocol, bearer, host)
	if err != nil {
		return fmt.Errorf("fetch apps for %s: %w", host, err)
	}
	if view == nil {
		return fmt.Errorf("%s: not paired or not visible to this bearer", host)
	}

	if jsonOut {
		b, _ := json.MarshalIndent(view, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	statusBits := []string{}
	if view.Online {
		statusBits = append(statusBits, "online")
	} else {
		statusBits = append(statusBits, "offline")
	}
	if view.Shared {
		statusBits = append(statusBits, "shared")
	}
	fmt.Printf("Host %s (%s)\n", view.Host, strings.Join(statusBits, ", "))
	if len(view.Apps) == 0 {
		fmt.Println("  no apps in cloudbox's last poll snapshot")
		return nil
	}
	fmt.Printf("%-20s  %-8s  %-13s  %s\n", "NAME", "SCHEME", "REQUIRE_LOGIN", "INDEX_PATH")
	for _, a := range view.Apps {
		fmt.Printf("%-20s  %-8s  %-13t  %s\n",
			a.Name, a.Scheme, a.RequireLogin, a.IndexPath)
	}
	return nil
}

// fetchRemoteHostApps GETs cloudbox's /api/v1/hosts (the same endpoint
// that powers `outpost ssh-config`), filters to the matching host, and
// returns its app catalog. Returns (nil, nil) when the host isn't
// visible to the bearer — caller distinguishes that from an error.
//
// This piggybacks on the existing endpoint rather than adding a
// per-host sibling: the listing is small (<1KB even for 50 hosts) and
// already carries every field we need. Same trust model as
// fetchSSHHosts, same ssh:read scope.
func fetchRemoteHostApps(ctx context.Context, server string, port int, protocol, token, host string) (*remoteHostView, error) {
	s := strings.TrimSpace(server)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(protocol), "wss") || strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if u.Port() == "" && port > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(port)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/hosts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Hosts []remoteHostView `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	for i := range out.Hosts {
		if strings.EqualFold(out.Hosts[i].Host, host) {
			return &out.Hosts[i], nil
		}
	}
	return nil, nil
}

func appsAddCmd() *cobra.Command {
	var (
		url, scheme, host, socket, indexPath, icon string
		port                                       int
		requireLogin, trustCloudIdentity           bool
		lanOnly                                    []string
		disabled                                   bool
		offline                                    bool
		jsonOut                                    bool
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update a custom app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ac := conf.AppConfig{
				Name:               args[0],
				Icon:               icon,
				Scheme:             scheme,
				Host:               host,
				Port:               port,
				Socket:             socket,
				IndexPath:          indexPath,
				LANOnlyPaths:       lanOnly,
				RequireLogin:       requireLogin,
				TrustCloudIdentity: trustCloudIdentity,
				Enabled:            !disabled,
			}
			params := admincore.AppUpsertParams{AppConfig: ac, URL: url}
			if offline {
				return runAppUpsertOffline(params, jsonOut)
			}
			return runAppUpsertViaMCP(cmd.Context(), params, jsonOut)
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "Target URL (alternative to --scheme/--host/--port/--socket)")
	cmd.Flags().StringVar(&scheme, "scheme", "", "http | https | tcp | unix | npipe")
	cmd.Flags().StringVar(&host, "host", "", "Target host (default 127.0.0.1 for TCP schemes)")
	cmd.Flags().IntVar(&port, "port", 0, "Target port (required for TCP schemes)")
	cmd.Flags().StringVar(&socket, "socket", "", "Socket path (required for unix / npipe)")
	cmd.Flags().StringVar(&icon, "icon", "", "URL to an icon image shown next to the tile in cloudbox")
	cmd.Flags().StringVar(&indexPath, "index-path", "", "Landing sub-path the cloudbox SPA prepends")
	cmd.Flags().StringSliceVar(&lanOnly, "lan-only-path", nil, "Path prefix 404'd on cloud requests (repeatable)")
	cmd.Flags().BoolVar(&requireLogin, "require-login", false, "Refuse cloud requests that haven't cleared /elevate")
	cmd.Flags().BoolVar(&trustCloudIdentity, "trust-cloud-identity", false, "Forward cloudbox-vouched identity as Remote-User / Remote-Email")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "Persist the entry but skip live registration")
	cmd.Flags().BoolVar(&offline, "offline", false, "Mutate the FileConfig directly without contacting the daemon (installer-script mode)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a one-line summary")
	return cmd
}

func runAppUpsertViaMCP(ctx context.Context, params admincore.AppUpsertParams, jsonOut bool) error {
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var out struct {
		OK  bool           `json:"ok"`
		App conf.AppConfig `json:"app"`
	}
	if err := session.callTool(ctx, "outpost_upsert_app", params, &out); err != nil {
		return err
	}
	return printAppUpsertResult(out.App, jsonOut)
}

func runAppUpsertOffline(params admincore.AppUpsertParams, jsonOut bool) error {
	core, err := offlineCore()
	if err != nil {
		return err
	}
	ac, err := core.UpsertApp(params)
	if err != nil {
		return err
	}
	return printAppUpsertResult(ac, jsonOut)
}

func printAppUpsertResult(ac conf.AppConfig, jsonOut bool) error {
	if jsonOut {
		b, _ := json.MarshalIndent(map[string]any{"ok": true, "app": ac}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	target := ac.Socket
	if target == "" {
		target = fmt.Sprintf("%s:%d", ac.Host, ac.Port)
	}
	fmt.Printf("Saved app %q (%s://%s) enabled=%t\n", ac.Name, ac.Scheme, target, ac.Enabled)
	return nil
}

func appsRmCmd() *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a custom app by name (idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if offline {
				core, err := offlineCore()
				if err != nil {
					return err
				}
				if err := core.DeleteApp(args[0]); err != nil {
					return err
				}
				fmt.Printf("Removed app %q\n", args[0])
				return nil
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			if err := session.callTool(cmd.Context(), "outpost_delete_app", map[string]any{"name": args[0]}, nil); err != nil {
				return err
			}
			fmt.Printf("Removed app %q\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "Mutate FileConfig directly without contacting the daemon")
	return cmd
}

func appsRotateTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate-token <name>",
		Short: "Rotate an app's provisioning bearer (trust_cloud_identity must be on)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				ProvisioningToken string `json:"provisioning_token"`
			}
			if err := session.callTool(cmd.Context(), "outpost_rotate_app_token", map[string]any{"name": args[0]}, &out); err != nil {
				return err
			}
			fmt.Println(out.ProvisioningToken)
			return nil
		},
	}
	return cmd
}

// appsSecretCmd prints the current SSO HMAC secret for one app so the
// operator can paste it into the cooperating app's config (one-time
// bootstrap; the value is also persisted in agent.json mode 0600).
func appsSecretCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "secret <name>",
		Short: "Print an app's SSO HMAC secret for pasting into the cooperating app's config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				SSOSecret string `json:"sso_secret"`
			}
			if err := session.callTool(cmd.Context(), "outpost_get_app_sso_secret", map[string]any{"name": args[0]}, &out); err != nil {
				return err
			}
			fmt.Println(out.SSOSecret)
			return nil
		},
	}
}

// appsRotateSecretCmd mints a new SSO HMAC secret for one app. The
// cooperating app stops verifying signatures until the operator pastes
// the new value into its config — same trade-off as `rotate-token`.
func appsRotateSecretCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-secret <name>",
		Short: "Rotate an app's SSO HMAC secret (trust_cloud_identity must be on)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				SSOSecret string `json:"sso_secret"`
			}
			if err := session.callTool(cmd.Context(), "outpost_rotate_app_sso_secret", map[string]any{"name": args[0]}, &out); err != nil {
				return err
			}
			fmt.Println(out.SSOSecret)
			return nil
		},
	}
}

func appsSuggestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "suggest",
		Short: "List auto-detected apps the operator could register (well-known sockets, ycode manifest)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Suggestions []admincore.Suggestion `json:"suggestions"`
			}
			if err := session.callTool(cmd.Context(), "outpost_suggest_apps", map[string]any{}, &out); err != nil {
				return err
			}
			for _, s := range out.Suggestions {
				marker := ""
				if s.Existing {
					marker = "  (already registered)"
				}
				fmt.Printf("%-20s  %-8s  %-30s  %s%s\n", s.Name, s.Scheme, s.Socket+s.Host, s.Note, marker)
			}
			return nil
		},
	}
	return cmd
}

// offlineCore constructs a one-shot admincore.Server pointed at the
// on-disk FileConfig — used by `--offline` flags that need to mutate
// config before the daemon's ever started. No restart wiring, no live
// AppRegistry; persistence-only.
func offlineCore() (*admincore.Server, error) {
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	// Make sure parents exist so the first --offline call on a fresh
	// host succeeds rather than failing on a missing config dir.
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := conf.SaveFile(cfgPath, &conf.FileConfig{}); err != nil {
			return nil, fmt.Errorf("init config: %w", err)
		}
	}
	core, err := admincore.New(admincore.Deps{
		ConfigPath: cfgPath,
	})
	if err != nil {
		return nil, err
	}
	return core, nil
}
