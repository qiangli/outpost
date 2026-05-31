// Command outpost runs on a home host: it pairs with the portal and
// surfaces local apps (web, shell, desktop, clipboard) through a tunnel.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/adminui"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/mcpapi"
	"github.com/qiangli/outpost/internal/agent/ollama"
	"github.com/qiangli/outpost/internal/agent/otel"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
	"github.com/qiangli/outpost/internal/agent/portal"
	"github.com/qiangli/outpost/internal/agent/runtime"
	"github.com/qiangli/outpost/internal/agent/sysinfo"
	"github.com/qiangli/outpost/internal/agent/upgrade"
	"github.com/qiangli/outpost/internal/agent/userkube"
	"github.com/qiangli/outpost/internal/agent/vkpodman"
	"github.com/qiangli/outpost/internal/agent/ycode"
)

// defaultPortal is the public ai.dhnt.io address used when the user
// doesn't override it via --server or the interactive prompt.
const defaultPortal = "https://ai.dhnt.io"

func main() {
	// First instruction of main: record this invocation to a durable
	// file so we have post-mortem evidence even if a sandbox/hook
	// kills us before any normal logging fires. See trace.go.
	emitStartupTrace()

	build := agent.ReadBuildInfo()
	root := &cobra.Command{
		Use:   "outpost",
		Short: "Pair a home host with the portal and tunnel local apps to it",
		// cobra auto-adds `--version` when this is non-empty. The default
		// template prints "outpost version <short-commit>"; keep it close
		// to that with the Go version appended so `outpost --version`
		// stays a useful one-liner.
		Version: build.Short(),
	}
	root.SetVersionTemplate("outpost version {{.Version}}\n")
	// Persistent flags that target a remote outpost via MCP. Read by
	// dialMCP via the rootDialOpts package var. --host / --token are
	// the explicit one-shot path; --remote is the cached-credentials
	// path (set up via `outpost remote login <name>`).
	root.PersistentFlags().StringVar(&rootDialOpts.Host, "host", "",
		"Admin endpoint of the target outpost (host:port or http://host:port). Overrides $OUTPOST_HOST / $OUTPOST_ADMIN_ADDR.")
	root.PersistentFlags().StringVar(&rootDialOpts.Token, "token", "",
		"MCP bearer token for the target outpost. Overrides $OUTPOST_MCP_TOKEN and the local FileConfig.")
	root.PersistentFlags().StringVar(&rootDialOpts.Remote, "remote", os.Getenv("OUTPOST_REMOTE"),
		"Use a cached remote (~/.config/outpost/remotes/<name>.json). Set up with 'outpost remote login <name>'.")
	root.AddCommand(
		startCmd(), registerCmd(), stopCmd(),
		sshProxyCmd(), sshConfigCmd(), connectCmd(),
		outboundCmd(), jobsCmd(), fgCmd(), bgCmd(), killCmd(), runCmd(),
		clusterCmd(), departCmd(), poolCmd(), kubectlCmd(),
		// MCP-client CLI parity (Phase 1.5):
		appsCmd(), builtinsCmd(), configCmd(), statusCmd(), unpairCmd(), restartCmd(), mcpCmd(),
		remoteCmd(),
		docsCmd(), versionCmd(), upgradeCmd(), rollbackCmd(),
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func startCmd() *cobra.Command {
	var (
		addrFlag       string
		nameFlag       string
		serverAddrFlag string
		serverPortFlag int
		vncAddrFlag    string
		adminAddrFlag  string
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the local agent and dial the portal",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// One-shot migration from the legacy os.UserConfigDir() /
			// os.UserCacheDir() locations (~/Library/Application Support
			// + ~/Library/Caches on macOS; %AppData% + %LocalAppData%
			// on Windows). After this returns, agent.json and the cache
			// dir live at the canonical XDG-style location on every
			// platform.
			if _, err := conf.ResolveConfigPath(); err != nil {
				return fmt.Errorf("resolve config path: %w", err)
			}
			if _, err := conf.ResolveCacheDir(); err != nil {
				return fmt.Errorf("resolve cache dir: %w", err)
			}

			// Refuse to boot a second instance — the matrix tunnel uses
			// a fixed remote port, and two outposts fighting for the
			// same proxy slot is a recipe for confused users. Also
			// stamps a pidfile that `outpost stop` later resolves.
			if err := claimPidFile(); err != nil {
				return err
			}
			// Restart path clears the pidfile pre-emptively (so the child
			// can claim it) and skips this deferred removal.
			var restarting atomic.Bool
			defer func() {
				if !restarting.Load() {
					removePidFile()
				}
			}()

			cfg, err := conf.Load()
			if err != nil {
				return err
			}

			cfgPath, _ := conf.DefaultConfigPath()
			var fc *conf.FileConfig
			if cfgPath != "" {
				fc, _ = conf.LoadFile(cfgPath)
			}
			if fc == nil {
				fc = &conf.FileConfig{}
			}

			// Layer the on-disk FileConfig under whatever the env supplied,
			// then apply hardcoded defaults to fields still empty. CLI
			// flags are applied last so they win regardless.
			//
			// Precedence per field: CLI flag > env > FileConfig > default.
			//
			// FileConfig.ServerAddr / ServerPort are special — they're
			// portal-controlled (filled by `outpost register`), so the
			// file wins over env when present. The other fields fall
			// through normally.
			if cfg.AgentName == "" {
				cfg.AgentName = fc.AgentName
			}
			if cfg.Token == "" {
				cfg.Token = fc.Token
			}
			if cfg.RemotePort == 0 {
				cfg.RemotePort = fc.RemotePort
			}
			if fc.ServerAddr != "" {
				cfg.ServerAddr = fc.ServerAddr
			}
			if fc.ServerPort != 0 {
				cfg.ServerPort = fc.ServerPort
			}
			if cfg.Protocol == "" {
				cfg.Protocol = fc.Protocol
			}
			if cfg.AuthURL == "" {
				cfg.AuthURL = fc.AuthURL
			}
			if cfg.LocalAddr == "" {
				cfg.LocalAddr = fc.LocalAddr
			}
			if cfg.VNCAddr == "" {
				cfg.VNCAddr = fc.VNCAddr
			}
			if cfg.AdminAddr == "" {
				cfg.AdminAddr = fc.AdminAddr
			}
			if cfg.AdminUsers == "" && len(fc.AdminUsers) > 0 {
				cfg.AdminUsers = strings.Join(fc.AdminUsers, ",")
			}

			// Hardcoded defaults — applied only when nothing else
			// supplied a value.
			if cfg.LocalAddr == "" {
				cfg.LocalAddr = conf.DefaultLocalAddr
			}
			if cfg.VNCAddr == "" {
				cfg.VNCAddr = conf.DefaultVNCAddr
			}
			if cfg.ServerAddr == "" {
				cfg.ServerAddr = conf.DefaultServerAddr
			}
			if cfg.ServerPort == 0 {
				cfg.ServerPort = conf.DefaultServerPort
			}

			if addrFlag != "" {
				cfg.LocalAddr = addrFlag
			}
			if nameFlag != "" {
				cfg.AgentName = nameFlag
			}
			if serverAddrFlag != "" {
				cfg.ServerAddr = serverAddrFlag
			}
			if serverPortFlag != 0 {
				cfg.ServerPort = serverPortFlag
			}
			if vncAddrFlag != "" {
				cfg.VNCAddr = vncAddrFlag
			}
			if adminAddrFlag != "" {
				cfg.AdminAddr = adminAddrFlag
			}

			apps, err := buildAppRegistry(fc, cfg.Apps)
			if err != nil {
				return err
			}
			// Built-in local-daemon proxies (podman, ollama). The admin UI
			// toggle is the source of truth; we silently skip when the
			// daemon isn't actually reachable so a stale "enabled" flag
			// doesn't break boot.
			if fc.PodmanOn() {
				if bt := agent.DetectPodman(); bt.Available && bt.Socket != "" {
					if err := apps.RegisterFromConfig(conf.AppConfig{
						Name: agent.BuiltinPodman, Scheme: "unix", Socket: bt.Socket,
						Role: "admin", Enabled: true,
					}); err != nil {
						slog.Warn("podman builtin: register", "err", err)
					} else {
						slog.Info("podman builtin: registered", "socket", bt.Socket)
					}
				} else {
					slog.Warn("podman builtin enabled but daemon not detected — skipping")
				}
			}
			// Ollama pool service, populated when the built-in ollama
			// proxy registers below. Threaded into the watcher startup
			// later in this function so the same Counter feeds both the
			// /_pool/capacity probe and the push registry payload.
			var (
				ollamaSvc *ollama.Service
				ollamaURL string
			)
			if fc.OllamaOn() {
				if bt := agent.DetectOllama(); bt.Available && bt.URL != "" {
					ollamaURL = bt.URL
					u, perr := url.Parse(bt.URL)
					if perr == nil {
						host := u.Hostname()
						port := 0
						if p := u.Port(); p != "" {
							port, _ = strconv.Atoi(p)
						}
						if port == 0 {
							port = 80
						}
						if err := apps.RegisterFromConfig(conf.AppConfig{
							Name: agent.BuiltinOllama, Scheme: u.Scheme, Host: host, Port: port,
							Role: "admin", Enabled: true,
						}); err != nil {
							slog.Warn("ollama builtin: register", "err", err)
						} else {
							slog.Info("ollama builtin: registered", "target", bt.URL)
							// Decorate the registered ollama mount so the
							// pool router on cloudbox can find it (capabilities)
							// AND wire the per-request counter + capacity probe
							// (intercept + proxy wrap). Both pieces stay no-ops
							// when fc.OllamaPoolOn() is false — the watcher
							// is what actually publishes; the counter just
							// observes locally. The marginal cost of always
							// instrumenting is one atomic per generation
							// request, which is invisible next to the
							// upstream Ollama latency.
							apps.SetCapabilities(agent.BuiltinOllama, &agent.AppCapabilities{Type: "llm"})
							ollamaSvc = ollama.NewService(ollama.NewCounter())
							apps.SetProxyWrap(agent.BuiltinOllama, ollamaSvc.WrapProxy)
							apps.AddIntercept(agent.BuiltinOllama, "/_pool/capacity", ollamaSvc.CapacityHandler())
						}
					}
				} else {
					slog.Warn("ollama builtin enabled but daemon not detected — skipping")
				}
			}

			// OTel surfaces. ycode's bearer-authed proxy at 127.0.0.1:31415
			// fronts an embedded Prometheus / Alertmanager / VictoriaLogs /
			// Jaeger / Perses stack. We expose each as its own built-in app
			// — reusing the per-host matrix_elev cookie machinery — with
			// the ycode bearer injected by a SetProxyWrap so cloudbox-side
			// callers don't need to learn ycode credentials. The local
			// surfaces are also what DKS-deployed observability apps
			// scrape via the matrix tunnel when they need per-host drill-
			// down (the fleet-wide story is push-based remote_write
			// directly to cluster services — see FileConfig.Cluster.*).
			// Skips silently when ycode isn't running, mirroring the
			// podman/ollama "enabled but daemon absent" handling above.
			if fc.OtelOn() {
				if t := otel.Detect(); t.Available {
					wrap := otel.BearerInjector(t.Token)
					for _, surface := range otel.Surfaces() {
						target := t.ProxyURL + otel.SubPath(surface)
						if err := apps.RegisterWithMeta(
							surface, target,
							agent.AppMeta{
								RequireLogin: true,
								Capabilities: &agent.AppCapabilities{Type: surface},
							},
						); err != nil {
							slog.Warn("otel builtin: register", "surface", surface, "err", err)
							continue
						}
						slog.Info("otel builtin: registered", "surface", surface, "target", target)
						apps.SetProxyWrap(surface, wrap)
					}
				} else {
					slog.Warn("otel builtin enabled but ycode proxy not detected — skipping", "manifest", t.ManifestPath)
				}
			}

			// ycode UI share. When YcodeShareOn (default on whenever ycode
			// is enabled), iterate the ycode surfaces catalog and register
			// each opted-in surface as its own per-tile built-in app
			// (`ycode` for chat, `ycode-canvas`, `ycode-ollama`, etc.).
			// The per-surface overlay is FileConfig.YcodeShareSurfaces;
			// catalog defaults supply the rest. Cloudbox's DefaultApps
			// lists `ycode` so the chat tile renders automatically; the
			// other tiles appear once the operator opts in from the SPA.
			//
			// RequireLogin=false by default for these owner-targeted
			// agentic surfaces (matches custom-app conventions; cloudbox's
			// hostAuthGate still gates non-owners via HostShare). Operators
			// who want OS-password elevation flip ycode_share_require_login
			// — it applies to ALL ycode surfaces uniformly.
			if fc.YcodeOn() && fc.YcodeShareOn() {
				if t := otel.Detect(); t.Available {
					wrap := otel.BearerInjector(t.Token)
					requireLogin := fc.YcodeShareRequireLoginOn()
					for _, s := range otel.YcodeSurfaces() {
						if !otel.YcodeSurfaceEnabled(fc.YcodeShareSurfaces, s.Name) {
							continue
						}
						target := t.ProxyURL + s.Path
						if err := apps.RegisterWithMeta(
							s.Name, target,
							agent.AppMeta{
								RequireLogin: requireLogin,
								Capabilities: &agent.AppCapabilities{Type: s.Name},
							},
						); err != nil {
							slog.Warn("ycode builtin: register", "surface", s.Name, "err", err)
							continue
						}
						slog.Info("ycode builtin: registered", "surface", s.Name, "target", target)
						apps.SetProxyWrap(s.Name, wrap)
					}
				} else {
					slog.Warn("ycode share enabled but ycode proxy not detected — skipping", "manifest", t.ManifestPath)
				}
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// ycode supervisor: when YcodeEnabled is on, detect a
			// running `ycode serve` and start one if a binary is
			// installed but no daemon is up. Fire-and-forget in a
			// goroutine so a slow ycode boot (up to 30 s for the
			// readiness wait) doesn't block outpost startup — outpost
			// stays useful even if ycode is misconfigured.
			if fc.YcodeOn() {
				go func() {
					info, err := ycode.Start(ctx)
					switch {
					case err != nil:
						slog.Warn("ycode supervisor: start", "err", err, "state", info.State)
					case info.State == ycode.StateRunning:
						slog.Info("ycode supervisor: running",
							"endpoint", info.APIEndpoint, "version", info.Version)
					default:
						slog.Info("ycode supervisor: nothing to start",
							"state", info.State, "binary", info.BinaryPath)
					}
				}()
			}

			// restartFn is what the admin UI calls after a save that
			// requires the tunnel or built-in routes to reload (pairing,
			// agent name, built-in toggles). We flag the parent's exit
			// path so the deferred removePidFile becomes a no-op, then
			// cancel the context so g.Wait() returns and we re-exec.
			var shouldRestart atomic.Bool
			restartFn := func() {
				shouldRestart.Store(true)
				stop()
			}

			// AdminAddr already went through the env → file → default
			// → CLI layering above. Apply the package default last
			// (kept on the adminui side so a downstream import of just
			// the adminui package keeps the same default).
			adminAddr := cfg.AdminAddr
			if adminAddr == "" {
				adminAddr = adminui.DefaultAdminAddr
			}
			// Load (or generate + persist) the HMAC key for admin-UI
			// session cookies. Storing it in the FileConfig is what lets
			// sessions survive a built-in toggle's re-exec without
			// kicking the operator back to the login screen.
			sessionKey, err := conf.EnsureAdminSessionKey(cfgPath, fc)
			if err != nil {
				return fmt.Errorf("admin session key: %w", err)
			}
			// Outbound manager: derives the cloudbox HTTP base URL from
			// the pairing. ServerAddr/ServerPort/Protocol describe how
			// the matrix tunnel dials cloudbox; for plain HTTP API calls
			// we translate "wss" → "https" and "websocket" → "http".
			outbound := agent.NewOutboundManager(cloudboxHTTPBase(fc), fc.AccessToken, nil)
			outbound.Register(fc.Outbound)
			// Rehydrate persisted matrix_elev cookies from a previous
			// outpost lifetime so the mounts come back online without
			// the operator re-entering the OS password. Mounts that
			// never persisted a cookie (never Connected, or
			// Disconnected later) stay in cfg-only state. Safe to call
			// even when fc.Outbound is empty.
			outbound.AutoReconnect()

			// LLM-pool status closure: nil-safe wrapper around the
			// ollama service so the admin UI's /api/config refresh
			// can render a live pool diagnostic. When Ollama isn't
			// enabled (ollamaSvc nil) the closure returns the
			// zero view, which the SPA renders as "(disabled)".
			llmPoolStatus := func() admincore.LLMPoolStatusView {
				if ollamaSvc == nil {
					return admincore.LLMPoolStatusView{}
				}
				st := ollamaSvc.Status()
				return admincore.LLMPoolStatusView{
					Running:     st.Watcher.Running,
					LastPushAt:  st.Watcher.LastPushAt,
					LastModels:  st.Watcher.LastModels,
					PushCount:   st.Watcher.PushCount,
					LastError:   st.Watcher.LastError,
					MaxParallel: st.Capacity.MaxParallel,
					InFlight:    st.Capacity.InFlight,
					CloudboxURL: st.Watcher.CloudboxURL,
					OllamaURL:   st.Watcher.OllamaURL,
				}
			}

			// Construct the shared business-logic layer first. The same
			// admincore.Server instance feeds adminui (human SPA) and
			// — soon — mcpapi (agent tools), so the file-save mutex and
			// restart-debounce timer are shared across the two surfaces.
			core, err := admincore.New(admincore.Deps{
				ConfigPath:          cfgPath,
				Apps:                apps,
				Outbound:            outbound,
				Restart:             restartFn,
				CloudboxBase:        cloudboxHTTPBase(fc),
				CloudboxAccessToken: fc.AccessToken,
				AgentName:           fc.AgentName,
				LLMPoolStatus:       llmPoolStatus,
			})
			if err != nil {
				return fmt.Errorf("admincore: %w", err)
			}

			// Cloudbox-pushed self-upgrade worker + ledger. Constructed
			// before mcpapi.New so the same Worker/Ledger feed both the
			// MCP tools (outpost_rollback, outpost_upgrade_history) and
			// the POST /admin/upgrade route mounted on the main tunnel
			// server later in this function. Only wired for paired
			// hosts — an unpaired daemon has no matrix-tunnel secret
			// for cloudbox to authenticate with, and the MCP tools
			// won't register either (they check s.upgrader != nil).
			var (
				upgradeWorker *upgrade.Worker
				upgradeLedger *upgrade.Ledger
			)
			if fc.AccessToken != "" {
				cacheDir, _ := conf.ResolveCacheDir()
				ledgerPath := ""
				if cacheDir != "" {
					ledgerPath = filepath.Join(cacheDir, "upgrade.log")
				}
				upgradeLedger = upgrade.NewLedger(ledgerPath)
				pendingPath := upgrade.PendingPath(cacheDir)
				exe, _ := os.Executable()
				// Drop any <exe>.replaced-* siblings left behind by a
				// prior Windows-swap run. No-op on Unix. Idempotent.
				upgrade.CleanupStaleSwaps(exe)
				upgradeWorker, err = upgrade.NewWorker(upgrade.Options{
					State: func() upgrade.StateSnapshot {
						// Re-read the current FileConfig so a just-toggled
						// update_mode takes effect on the next /admin/upgrade
						// POST without a daemon restart.
						cur, _ := conf.LoadFile(cfgPath)
						if cur == nil {
							cur = fc
						}
						return upgrade.StateSnapshot{
							UpdateMode:    cur.UpdateModeName(),
							CurrentCommit: agent.ReadBuildInfo().Short(),
							BinaryPath:    exe,
							PendingPath:   pendingPath,
						}
					},
					Restart: core.ScheduleRestart,
					Ledger:  upgradeLedger,
				})
				if err != nil {
					return fmt.Errorf("upgrade worker: %w", err)
				}
				// Make the worker + ledger visible to admincore's
				// shared business-logic layer so the adminui Update
				// tab + MCP tools all read from the same source.
				core.AttachUpgrade(upgradeWorker, upgradeLedger)
			}

			// MCP server — same loopback listener as adminui, mounted
			// at /mcp/*. Bearer-token auth (FileConfig.MCPBearerToken,
			// auto-generated on first boot) is isolated from adminui's
			// session-cookie auth by path-prefix routing.
			mcpToken, err := conf.EnsureMCPBearerToken(cfgPath, fc)
			if err != nil {
				return fmt.Errorf("mcp bearer token: %w", err)
			}
			mcpSrv, err := mcpapi.New(mcpapi.Deps{
				Core:    core,
				Token:   mcpToken,
				Version: agent.ReadBuildInfo().Short(),
				RotateFn: func() (string, error) {
					// Re-load the FileConfig from disk so any other write
					// since boot is respected, then rotate atomically.
					cur, lerr := conf.LoadFile(cfgPath)
					if lerr != nil || cur == nil {
						cur = fc
					}
					return conf.RotateMCPBearerToken(cfgPath, cur)
				},
				Upgrader: upgradeWorker,
				Ledger:   upgradeLedger,
			})
			if err != nil {
				return fmt.Errorf("mcp api: %w", err)
			}

			adminSrv, err := adminui.New(adminui.Deps{
				Core:       core,
				ListenAddr: adminAddr,
				Auth:       hostauth.DefaultAuthenticator(),
				SessionKey: sessionKey,
				// MCP closures: the SPA's Pair tab uses these to show
				// the .mcp.json snippet (endpoint + bearer) and to
				// rotate the token when the operator clicks "Rotate".
				// Rotation goes through mcpapi.Rotate so the in-memory
				// token swap stays consistent with the persisted value.
				MCPToken:       mcpSrv.Token,
				RotateMCPToken: mcpSrv.Rotate,
				// On shutdown, close all MCP SSE sessions before
				// http.Server.Shutdown waits for in-flight requests
				// — otherwise the long-lived streamable-transport
				// SSE connections block teardown until the 5-second
				// timeout fires and `outpost stop` SIGKILLs.
				OnShutdown: mcpSrv.Close,
			})
			if err != nil {
				return fmt.Errorf("admin ui: %w", err)
			}
			adminSrv.Engine().Any("/mcp", gin.WrapH(mcpSrv.Handler()))
			adminSrv.Engine().Any("/mcp/*p", gin.WrapH(mcpSrv.Handler()))
			// Backfill the endpoint URL on the SPA-facing closure now
			// that the listener address is known.
			adminSrv.SetMCPEndpoint(adminSrv.URL() + "/mcp/")
			fmt.Fprintf(os.Stderr, "MCP endpoint: %s/mcp/  (bearer: %s)\n", adminSrv.URL(), mcpToken)

			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error {
				fmt.Fprintf(os.Stderr, "Admin UI: %s\n", adminSrv.URL())
				slog.Info("outpost: admin ui listening", "url", adminSrv.URL())
				return adminSrv.Serve(gctx)
			})

			if cfg.AgentName == "" {
				fmt.Fprintln(os.Stderr, "Not yet configured — open the Admin UI to pair this host with the portal.")
				slog.Info("outpost: awaiting first-run pairing through admin UI")
				if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
					return err
				}
				if shouldRestart.Load() {
					restarting.Store(true)
					removePidFile()
					return execSelfStart()
				}
				return nil
			}

			// Client-only registrations are a credential vehicle: the
			// machine uses agent.json for outbound SSH (`outpost
			// connect` / `outpost ssh-proxy`) but has no inbound
			// surface to expose. `outpost start` would otherwise dial
			// the matrix tunnel for nothing. Block here, surface the
			// admin UI for management, and wait on the context.
			if fc.ClientOnly {
				fmt.Fprintln(os.Stderr, "Outpost is registered in client-only mode — no matrix tunnel, no inbound routes.")
				fmt.Fprintln(os.Stderr, "Use `outpost connect <host>` / `outpost ssh-proxy <host>` to ssh out.")
				slog.Info("outpost: client-only mode; admin UI only")
				if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
					return err
				}
				if shouldRestart.Load() {
					restarting.Store(true)
					removePidFile()
					return execSelfStart()
				}
				return nil
			}

			admins := agent.NewAdminSet(cfg.AdminUsers)

			// Load the SSH host key lazily — only when the SSH builtin is
			// on. The key file is generated on first use (ed25519) and
			// persists across re-pairings so clients' known_hosts entries
			// stay valid.
			var sshHostKey ssh.Signer
			if fc.SSHOn() {
				k, kerr := agent.LoadOrCreateHostKey()
				if kerr != nil {
					return fmt.Errorf("ssh host key: %w", kerr)
				}
				sshHostKey = k
			}

			// Peer-host registry caches /api/v1/ssh/hosts so the SSH
			// direct-tcpip allowlist (used for `ssh -J` ProxyJump
			// between paired outposts) doesn't have to round-trip
			// cloudbox per channel. Nil when unpaired — the SSH server
			// then keeps the loopback-only posture.
			var peers *peerhosts.Registry
			if fc.AccessToken != "" {
				peers = peerhosts.New(peerhosts.Config{
					ServerAddr: cfg.ServerAddr,
					ServerPort: cfg.ServerPort,
					Protocol:   cfg.Protocol,
					Token:      fc.AccessToken,
				})
			}

			engine := gin.Default()
			agent.RegisterRoutes(engine.Group("/"), agent.Deps{
				AgentName:             cfg.AgentName,
				Apps:                  apps,
				Admins:                admins,
				AuthURL:               cfg.AuthURL,
				VNCAddr:               cfg.VNCAddr,
				ShellDisabled:         !fc.ShellOn(),
				DesktopDisabled:       !fc.DesktopOn(),
				ClipboardDisabled:     !fc.ClipboardOn(),
				SSHDisabled:           !fc.SSHOn(),
				SSHAllowLocalForward:  fc.SSHAllowLocalForwardOn(),
				SSHAllowRemoteForward: fc.SSHAllowRemoteForwardOn(),
				SSHAllowAgentForward:  fc.SSHAllowAgentForwardOn(),
				SFTPEnabled:           fc.SFTPOn(),
				SSHHostKey:            sshHostKey,
				PeerHosts:             peers,
				SSHForwardSockets:     fc.SSHForwardSockets,
				CloudboxBase:          cloudboxHTTPBase(fc),
				CloudboxProtocol:      cfg.Protocol,
				AccessToken:           fc.AccessToken,
				SelfName:              cfg.AgentName,
				MountUpgradeRoute: func(rg *gin.RouterGroup) {
					upgrade.MountRoute(rg, upgradeWorker)
				},
				UpdateMode: func() string {
					cur, _ := conf.LoadFile(cfgPath)
					if cur == nil {
						cur = fc
					}
					return cur.UpdateModeName()
				},
				SystemInfo: func() any {
					dir, _ := conf.DefaultCacheDir()
					return sysinfo.Collect(dir)
				},
			})

			// Bind the local listener first so we know its port before
			// telling the matrix-tunnel server how to reach us.
			ln, err := net.Listen("tcp", cfg.LocalAddr)
			if err != nil {
				return fmt.Errorf("local listen: %w", err)
			}
			localPort := ln.Addr().(*net.TCPAddr).Port

			localSrv := &http.Server{
				Handler:           engine,
				ReadHeaderTimeout: 10 * time.Second,
			}

			// When cluster mode is on AND Mode="agent", register an
			// STCP visitor for the cloudbox-side apiserver publisher
			// (see hub/internal/tunnel/serverdial.go). This opens a
			// loopback listener that `k3s agent` dials as if the
			// apiserver were local — the matrix tunnel carries the
			// bytes to cloudbox, no new public port needed. Names
			// here MUST match the publisher's User + ProxyName ("cloudbox"
			// / "k3s-apiserver" — fixed by cloudbox-side constants).
			var visitors []agent.STCPVisitor
			if fc.ClusterOn() && fc.Cluster.ClusterModeAgent() {
				apiPort := fc.Cluster.K8sAPIPort
				if apiPort == 0 {
					apiPort = 6443
				}
				if fc.Cluster.STCPSecret == "" {
					slog.Warn("cluster mode=agent: STCPSecret empty; visitor disabled (re-pair to fetch from cloudbox)")
				} else {
					visitors = append(visitors, agent.STCPVisitor{
						Name:       "k3s-apiserver-visitor",
						ServerUser: "cloudbox",
						ServerName: "k3s-apiserver",
						Secret:     fc.Cluster.STCPSecret,
						BindAddr:   "127.0.0.1",
						BindPort:   apiPort,
					})
				}
			}

			proxies := []agent.TCPProxy{{
				Name:       cfg.AgentName + "-http",
				LocalIP:    "127.0.0.1",
				LocalPort:  localPort,
				RemotePort: cfg.RemotePort,
			}}
			// Phase 2: publish kubelet :10250 so cloudbox's embedded
			// apiserver can dial through 127.0.0.1:<KubeletProxyPort>
			// for `kubectl logs/exec`. Only registered in Mode=agent —
			// vkpodman doesn't have a local kubelet to proxy. A
			// KubeletProxyPort==0 means cloudbox didn't allocate one
			// (pool exhausted, or running an older cloudbox); the
			// outpost just doesn't publish the proxy and the apiserver
			// keeps using whatever address kubelet self-reports (which
			// won't resolve from cloudbox — `kubectl logs` will fail,
			// but the rest of cluster-agent mode works).
			if fc.ClusterOn() && fc.Cluster.ClusterModeAgent() && fc.Cluster.KubeletProxyPort > 0 {
				proxies = append(proxies, agent.TCPProxy{
					Name:       cfg.AgentName + "-kubelet",
					LocalIP:    "127.0.0.1",
					LocalPort:  10250,
					RemotePort: fc.Cluster.KubeletProxyPort,
				})
			}

			tunnel, err := agent.NewTunnel(agent.TunnelConfig{
				ServerAddr: cfg.ServerAddr,
				ServerPort: cfg.ServerPort,
				Protocol:   cfg.Protocol,
				Token:      cfg.Token,
				User:       cfg.AgentName,
			}, proxies, visitors)
			if err != nil {
				return err
			}

			g.Go(func() error {
				slog.Info("matrix-agent: local http listening", "addr", ln.Addr().String(), "name", cfg.AgentName, "apps", apps.Names())
				if err := localSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			})
			g.Go(func() error {
				slog.Info("matrix-agent: dialing matrix tunnel", "server", cfg.ServerAddr, "port", cfg.ServerPort, "remotePort", cfg.RemotePort)
				return tunnel.Run(gctx)
			})
			// Ollama pool watcher — only spins up when the user opted
			// into the shared pool AND the local Ollama proxy actually
			// registered (ollamaSvc != nil). The watcher itself tolerates
			// an empty access_token (just blocks on ctx) so unpaired
			// hosts in a half-configured state don't crash; the gate
			// here is mostly to avoid the "watcher started but did
			// nothing" log noise.
			if fc.OllamaPoolOn() && ollamaSvc != nil {
				cbBase := cloudboxHTTPBase(fc)
				if cbBase == "" {
					slog.Warn("ollama pool: cloudbox URL is empty — watcher disabled")
				} else {
					if ollamaURL == "" {
						ollamaURL = "http://127.0.0.1:11434"
					}
					w, werr := ollama.New(ollama.Config{
						AgentName:   cfg.AgentName,
						Version:     agent.ReadBuildInfo().Short(),
						OllamaURL:   ollamaURL,
						CloudboxURL: cbBase,
						AccessToken: fc.AccessToken,
						Capacity:    ollamaSvc.Counter(),
					})
					if werr != nil {
						slog.Warn("ollama pool: watcher init failed", "err", werr)
					} else {
						// Hand the watcher to the service so Status()
						// reports last-push state + capacity through
						// one accessor.
						ollamaSvc.SetWatcher(w)
						g.Go(func() error {
							if err := w.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
								slog.Warn("ollama pool: watcher exited", "err", err)
							}
							return nil
						})
					}
				}
			}
			// Cluster mode: join the cloudbox virtual-podman cluster as a
			// virtual node. Independent of the PodmanEnabled reverse-proxy
			// app — cluster mode dials the local podman socket directly
			// for libpod calls and doesn't need the /app/podman/* route to
			// be mounted. Boot errors are logged but never fatal: a
			// half-configured cluster section shouldn't prevent the rest
			// of outpost from running.
			if fc.ClusterOn() {
				// Mode=agent: launch the real `k3s agent` subprocess
				// against the loopback STCP visitor wired into the
				// matrix tunnel above. vkpodman stays disabled in this
				// mode — outposts can't run both the v1 virtual-kubelet
				// AND a real kubelet on the same Node identity.
				if fc.Cluster.ClusterModeAgent() {
					if err := startK3sAgentRunner(gctx, g, fc); err != nil {
						slog.Warn("cluster mode=agent: disabled", "err", err)
					}
				} else if err := startClusterRunner(gctx, g, fc, cfgPath, apps); err != nil {
					slog.Warn("cluster mode: disabled", "err", err)
				}
				// Materialize the kubectl-ready kubeconfig on disk so
				// the operator gets `kubectl get nodes` and `helm list`
				// without running anything extra. Failures land in
				// userkube.LastStatus (visible in the admin UI's
				// Cluster section); we don't gate vkpodman startup on
				// success — agent-side credentials work independently.
				if path, err := userkube.FetchAndWrite(gctx, cloudboxHTTPBase(fc), fc.AccessToken, fc.ClusterNodeName(), ""); err != nil {
					slog.Warn("cluster mode: user kubeconfig write failed (admin UI will show the error)", "err", err, "path", path)
				} else {
					slog.Info("cluster mode: user kubeconfig ready", "path", path)
				}
			}
			g.Go(func() error {
				<-gctx.Done()
				slog.Info("matrix-agent: shutting down")
				// Phase 2b cooperative-failover hook. Fire-and-forget
				// ping cloudbox BEFORE tearing down the matrix tunnel
				// so cluster-svc stops routing new traffic to our pods
				// in the seconds it'll take us to actually disconnect.
				// Hard 3s budget — a slow/unresponsive cloudbox can't
				// hold up our shutdown. Skipped silently when cluster
				// mode is off or the prerequisites (access_token,
				// cloudbox URL, agent name) are missing.
				if fc.ClusterOn() && fc.AccessToken != "" && fc.AgentName != "" {
					notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 3*time.Second)
					if err := notifyDeparting(notifyCtx, cloudboxHTTPBase(fc), fc.AccessToken, fc.AgentName); err != nil {
						slog.Warn("matrix-agent: departure notification failed (continuing shutdown)", "err", err)
					} else {
						slog.Info("matrix-agent: departure notified", "agent", fc.AgentName)
					}
					notifyCancel()
				}
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = localSrv.Shutdown(shutdownCtx)
				tunnel.Close()
				return nil
			})
			err = g.Wait()
			if shouldRestart.Load() {
				restarting.Store(true)
				removePidFile()
				return execSelfStart()
			}
			return err
		},
	}
	cmd.Flags().StringVar(&addrFlag, "addr", "", "Local loopback HTTP listen address (overrides $AGENT_ADDR)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "Agent name displayed in the portal (overrides $AGENT_NAME)")
	cmd.Flags().StringVar(&serverAddrFlag, "server", "", "matrix-tunnel server host (overrides $MATRIX_SERVER_ADDR)")
	cmd.Flags().IntVar(&serverPortFlag, "server-port", 0, "matrix-tunnel server port (overrides $MATRIX_SERVER_PORT)")
	cmd.Flags().StringVar(&vncAddrFlag, "vnc-addr", "", "VNC server to expose for the desktop tab (overrides $AGENT_VNC_ADDR / FileConfig.VNCAddr; default 127.0.0.1:5900)")
	cmd.Flags().StringVar(&adminAddrFlag, "admin-addr", "", "Admin UI listen address (overrides $OUTPOST_ADMIN_ADDR; default 127.0.0.1:17777). Use 0.0.0.0:17777 to expose to the LAN; login is then always required.")
	return cmd
}

// notifyDeparting POSTs /api/v1/cluster/departing on cloudbox to
// flag this outpost as about-to-leave. Bearer-authed with the same
// access_token the outpost already carries for /api/cluster/agent;
// header X-Outpost-Agent names the host so a compromised SA can't
// depart a peer. Cloudbox's HostRegistry.Lookup then returns
// ok=false for the next 30 s, which makes cluster-svc's pre-filter
// skip pods on this node — graceful failover BEFORE the tunnel
// actually drops.
//
// All errors are non-fatal: shutdown proceeds regardless. The
// reactive ~40 s NodeNotReady path is still the fallback. This is
// strictly the "make the common predictable case ~0 s" hook.
func notifyDeparting(ctx context.Context, cloudboxBase, accessToken, agentName string) error {
	base := strings.TrimRight(strings.TrimSpace(cloudboxBase), "/")
	if base == "" {
		return errors.New("notifyDeparting: empty cloudboxBase")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/v1/cluster/departing", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Outpost-Agent", agentName)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("cloudbox responded %d", resp.StatusCode)
	}
	return nil
}

// startK3sAgentRunner launches the real `k3s agent` subprocess
// (k3sagent.Run) that joins the cloudbox-embedded apiserver via the
// STCP visitor wired into the matrix tunnel. Phase 1 of the
// "real shared k8s" plan — replaces the v1 vkpodman virtual-kubelet
// path when fc.Cluster.Mode == "agent".
//
// Setup errors return; the long-running loop's errors flow through the
// errgroup the same way the tunnel's do. Like startClusterRunner, we
// never make a cluster-mode boot failure fatal to the outpost.
func startK3sAgentRunner(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig) error {
	cc := fc.Cluster
	if cc == nil {
		return errors.New("cluster mode=agent: no Cluster config")
	}
	if cc.NodeToken == "" {
		return errors.New("cluster mode=agent: NodeToken empty (re-pair to refresh)")
	}
	if cc.STCPSecret == "" {
		return errors.New("cluster mode=agent: STCPSecret empty (re-pair to refresh)")
	}
	apiPort := cc.K8sAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}
	nodeName := fc.ClusterNodeName()
	if nodeName == "" {
		return errors.New("cluster mode=agent: NodeName empty (agent_name unset?)")
	}

	// Refactor (post-Phase 3): the kubelet runtime moved into a
	// privileged podman container so the same model works on Linux
	// (native) AND macOS/Windows (Docker Desktop / Rancher Desktop /
	// ycode-podman / Lima). The outpost daemon stays on the host;
	// only the container hosts k3s-agent + tailscaled + CNI. One
	// outpost = one identity = one Node (named after the outpost).
	rtOpts := runtime.Options{
		AgentName:          nodeName,
		NodeToken:          cc.NodeToken,
		APIServer:          fmt.Sprintf("https://127.0.0.1:%d", apiPort),
		APIPort:            apiPort,
		CloudboxHost:       fc.ServerAddr,
		CloudboxPort:       fc.ServerPort,
		STCPSecret:         cc.STCPSecret,
		MatrixToken:        fc.Token,
		PodCIDR:            cc.OverlayPodCIDR,
		OverlayLoginServer: cc.OverlayLoginServer,
		OverlayAuthKey:     cc.OverlayAuthKey,
	}
	if err := runtime.Up(ctx, rtOpts); err != nil {
		if errors.Is(err, runtime.ErrPodmanNotFound) {
			slog.Warn("cluster mode=agent: " + err.Error())
			return nil
		}
		slog.Warn("cluster mode=agent: runtime start failed", "err", err)
		return nil
	}
	g.Go(func() error {
		// Tail the container's logs into outpost's slog stream.
		// runtime.TailLogs returns when the container exits OR ctx
		// fires; either way the supervisor loop on the next
		// cluster-mode toggle restarts via Up().
		_ = runtime.TailLogs(ctx, rtOpts)
		return nil
	})
	return nil
}

// startClusterRunner validates fc.Cluster, detects the local podman
// socket, and launches vkpodman.Run on g. Setup errors return; the
// long-running loop's errors flow through the errgroup the same way
// the tunnel's do.
//
// We never make a cluster-mode boot failure fatal to the agent: a
// half-configured Cluster section shouldn't stop the matrix tunnel or
// admin UI from coming up. The caller logs the returned error and
// moves on.
func startClusterRunner(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig, cfgPath string, apps *agent.AppRegistry) error {
	nodeName := fc.ClusterNodeName()
	if nodeName == "" {
		return errors.New("ClusterNodeName empty (agent_name unset?)")
	}
	bt := agent.DetectPodman()
	if !bt.Available || bt.Socket == "" {
		return fmt.Errorf("podman socket not detected (tried %s)", bt.Socket)
	}

	// Bootstrap: if we have an outpost access_token and either no
	// persisted credentials yet or an already-expired ones, ask
	// cloudbox to mint a fresh kubeconfig and persist it. This is the
	// "operator flipped the toggle, never pasted anything" path.
	//
	// A fetch failure here is non-fatal when we already have cached
	// credentials (even expired ones — the refresher will retry on
	// the loop), and fatal otherwise (there's nothing to dial the
	// apiserver with).
	cloudboxBase := cloudboxHTTPBase(fc)
	if shouldFetchKubeconfig(fc) && fc.AccessToken != "" && cloudboxBase != "" {
		slog.Info("cluster mode: fetching kubeconfig from cloudbox", "node", nodeName, "cloudbox", cloudboxBase)
		fetched, err := vkpodman.FetchKubeconfig(ctx, cloudboxBase, fc.AccessToken, nodeName)
		if err != nil {
			if fc.Cluster == nil || fc.Cluster.Token == "" {
				return fmt.Errorf("cluster mode: fetch kubeconfig: %w", err)
			}
			slog.Warn("cluster mode: fetch failed; falling back to cached credentials",
				"err", err, "node", nodeName)
		} else {
			persistClusterCredential(fc, cfgPath, fetched)
		}
	}

	cc := fc.Cluster
	if cc == nil || cc.APIURL == "" || cc.Token == "" {
		return errors.New("cluster mode: no usable credentials (no access_token to fetch with, and no pasted kubeconfig either)")
	}

	// Write the live bearer token to a file so client-go's transport
	// re-reads it across rotations. Path stays under the agent's
	// cache dir (mode 0600 — same locality as the pidfile + logs).
	tokenFile, err := vkpodman.DefaultTokenFilePath()
	if err != nil {
		return fmt.Errorf("cluster mode: token-file path: %w", err)
	}
	if err := vkpodman.WriteTokenFile(tokenFile, cc.Token); err != nil {
		return fmt.Errorf("cluster mode: write token-file: %w", err)
	}

	kubeCfg, err := vkpodman.ConfigFromCluster(cc.APIURL, tokenFile, cc.CA)
	if err != nil {
		return err
	}

	// Namespace-access gate. Derive the outpost owner's email from
	// the access_token's email claim (set by cloudbox at register
	// time), compute the owner's per-user namespace via the same
	// formula cloudbox uses, and pass it to the Provider. nil
	// Access = no check; we'd hit that only if access_token isn't a
	// JWT (e.g. very old paired outposts, before the JWT format) —
	// log loudly and let the runner proceed in permissive mode so
	// the cluster path still works for legacy installs.
	var access *vkpodman.Access
	if fc.AccessToken != "" {
		if owner, err := vkpodman.OwnerEmailFromAccessToken(fc.AccessToken); err == nil {
			ns := vkpodman.NamespaceForEmail(owner)
			access = vkpodman.NewAccess(ns)
			slog.Info("cluster mode: namespace access gate", "owner", owner, "namespace", ns)
		} else {
			slog.Warn("cluster mode: could not derive owner from access_token; namespace check disabled (legacy token?)", "err", err)
		}
	}

	g.Go(func() error {
		slog.Info("cluster mode: joining", "node", nodeName, "apiserver", cc.APIURL, "podman_socket", bt.Socket)
		if err := vkpodman.Run(ctx, vkpodman.RunOptions{
			NodeName:      nodeName,
			PodmanSocket:  bt.Socket,
			Kube:          kubeCfg,
			Access:        access,
			TransientApps: appsAsTransient{apps},
		}); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("cluster mode: runner exited", "err", err)
		}
		return nil
	})

	// Token rotation. Only spin the refresher when we have a working
	// fetch path (access_token + cloudbox URL); when the operator
	// pasted a kubeconfig for a non-cloudbox cluster, leave the token
	// alone — they're responsible for replacing it before expiry.
	if fc.AccessToken != "" && cloudboxBase != "" {
		refresher := vkpodman.NewRefresher(vkpodman.RefreshDeps{
			CloudboxBase:  cloudboxBase,
			AccessToken:   fc.AccessToken,
			NodeName:      nodeName,
			TokenFilePath: tokenFile,
			OnRotation: func(p *vkpodman.ParsedKubeconfig) {
				persistClusterCredential(fc, cfgPath, p)
			},
		})
		g.Go(func() error {
			return refresher.Run(ctx, cc.Token)
		})
	}

	// Namespace allow-set refresh. Cloudbox is the source of truth for
	// which sharee namespaces may schedule on this node (owner +
	// HostShare rows with app="podman"). We do one synchronous fetch
	// before the runner starts to populate the gate, then spawn a
	// background loop that keeps it current. Failures never propagate
	// up: the owner-only set stays in place until the next successful
	// fetch (see access_refresh.go for the backoff policy).
	if access != nil && fc.AccessToken != "" && cloudboxBase != "" {
		if resp, err := vkpodman.FetchAccess(ctx, cloudboxBase, fc.AccessToken, nodeName); err == nil {
			access.Set(resp.AllowedNamespaces...)
			slog.Info("cluster mode: initial access refresh",
				"node", nodeName, "namespaces", resp.AllowedNamespaces)
		} else {
			slog.Warn("cluster mode: initial access fetch failed (will retry on loop)",
				"node", nodeName, "err", err)
		}
		accessRefresher := vkpodman.NewAccessRefresher(vkpodman.AccessRefreshDeps{
			CloudboxBase: cloudboxBase,
			AccessToken:  fc.AccessToken,
			NodeName:     nodeName,
			Access:       access,
		})
		g.Go(func() error { return accessRefresher.Run(ctx) })
	}

	return nil
}

// shouldFetchKubeconfig reports whether the boot path should ask
// cloudbox for fresh credentials before joining. True when there's no
// token at all (first-ever join) or when the persisted token has
// already expired (an outpost that was off long enough to miss its
// refresh window). Otherwise we trust the cached creds and let the
// refresher catch up on its own schedule.
func shouldFetchKubeconfig(fc *conf.FileConfig) bool {
	if fc.Cluster == nil || fc.Cluster.Token == "" {
		return true
	}
	exp := vkpodman.TokenExpiry(fc.Cluster.Token)
	if exp.IsZero() {
		// Non-JWT token (e.g. pasted from a non-cloudbox cluster).
		// Don't auto-fetch — that path is operator-managed.
		return false
	}
	return time.Now().After(exp)
}

// persistClusterCredential writes a freshly-fetched (or rotated)
// kubeconfig triple back into fc.Cluster + saves the FileConfig so a
// future outpost restart starts from the rotated state without
// re-fetching. Best-effort: a save failure logs but doesn't undo the
// in-memory rotation (the next refresh tick will try again).
func persistClusterCredential(fc *conf.FileConfig, cfgPath string, p *vkpodman.ParsedKubeconfig) {
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{Enabled: true}
	}
	fc.Cluster.APIURL = p.APIURL
	fc.Cluster.Token = p.Token
	fc.Cluster.CA = p.CA
	if cfgPath == "" {
		return
	}
	if err := conf.SaveFile(cfgPath, fc); err != nil {
		slog.Warn("cluster mode: rotated credentials but file save failed",
			"err", err, "path", cfgPath)
	}
}

// cloudboxHTTPBase derives the HTTP(S) base URL of cloudbox from the
// matrix-tunnel pairing fields. The same hostname serves both the wss
// tunnel and the HTTP API — protocols are just paired (wss↔https,
// websocket↔http, tcp↔http).
func cloudboxHTTPBase(fc *conf.FileConfig) string {
	if fc == nil || fc.ServerAddr == "" {
		return ""
	}
	scheme := "https"
	switch strings.ToLower(fc.Protocol) {
	case "wss":
		scheme = "https"
	case "ws", "websocket", "tcp", "":
		// For local dev (ws+18080) the API is plain HTTP on the same port.
		scheme = "http"
	}
	port := ""
	if fc.ServerPort != 0 && !((scheme == "https" && fc.ServerPort == 443) || (scheme == "http" && fc.ServerPort == 80)) {
		port = fmt.Sprintf(":%d", fc.ServerPort)
	}
	return scheme + "://" + fc.ServerAddr + port
}

// appsAsTransient bridges *agent.AppRegistry to vkpodman.TransientApps.
// Each transient pod registration uses RequireLogin: false because
// cluster-mode auth happens at cloudbox's /api/cluster/svc/* entry
// point (TokenReview-gated), not via the per-app elevation cookie
// the default AppRegistry.Register would require. The flag is the
// only deviation from the default Register path; everything else
// (scheme parsing, URL validation, mutual-exclusion with tcp-mode
// names) is inherited.
type appsAsTransient struct{ r *agent.AppRegistry }

func (a appsAsTransient) Register(name, target string) error {
	if a.r == nil {
		return nil
	}
	return a.r.RegisterWithMeta(name, target, agent.AppMeta{RequireLogin: false})
}

func (a appsAsTransient) Unregister(name string) {
	if a.r == nil {
		return
	}
	a.r.Unregister(name)
}

// buildAppRegistry seeds the live AppRegistry from whichever app source
// is authoritative. Order of precedence:
//  1. fc.Apps (structured config saved through the admin UI). Even an
//     empty slice wins — the user has explicitly said "no apps".
//  2. MATRIX_APPS env (legacy "name=url,..." string).
//  3. Convenience default `ycode → http://127.0.0.1:8765` when both are
//     absent.
func buildAppRegistry(fc *conf.FileConfig, envSpecs string) (*agent.AppRegistry, error) {
	reg := agent.NewAppRegistry()
	if fc != nil && fc.Apps != nil {
		for _, ac := range fc.Apps {
			if err := reg.RegisterFromConfig(ac); err != nil {
				return nil, err
			}
		}
		return reg, nil
	}
	specs := strings.TrimSpace(envSpecs)
	if specs == "" {
		if err := reg.Register("ycode", "http://127.0.0.1:8765"); err != nil {
			return nil, err
		}
		return reg, nil
	}
	for entry := range strings.SplitSeq(specs, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, target, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("MATRIX_APPS entry %q: expected name=url", entry)
		}
		if err := reg.Register(strings.TrimSpace(name), strings.TrimSpace(target)); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func registerCmd() *cobra.Command {
	var (
		serverURL  string
		code       string
		name       string
		out        string
		authURL    string
		title      string
		assumeYes  bool
		clientOnly bool
	)
	cmd := &cobra.Command{
		Use:     "register",
		Aliases: []string{"pair"},
		Short:   "Pair this host with the portal (alias: `outpost pair`)",
		Long: `Exchange a one-time pairing code for a persistent agent config.

Run with no flags for an interactive prompt that walks you through pairing.
Or pass --code (and optionally --server / --name) to skip the prompts —
useful for installer scripts and CI. --server defaults to the official
portal and --name defaults to this machine's hostname (.local/.lan
suffix stripped), so a fresh paste from the cloudbox "Generate invite
code" dialog usually only needs --code.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Scripted mode kicks in the moment --code is set on the CLI:
			// installer scripts pipe stdin from somewhere unpredictable, so
			// we can't fall back to "ask the user" without surprising them.
			scripted := code != ""
			reader := bufio.NewReader(os.Stdin)

			// In scripted mode, apply the same defaults the interactive
			// prompt would offer for the optional flags so a one-liner
			// `outpost register --server <portal> --code <code>` Just
			// Works without forcing the operator to spell out --name.
			if scripted {
				if serverURL == "" {
					serverURL = defaultPortal
				}
				if name == "" {
					name = defaultHostName()
				}
			}

			for {
				if !scripted {
					if err := collectInputs(reader, &serverURL, &code, &name); err != nil {
						return err
					}
				}
				if serverURL == "" || code == "" || name == "" {
					return errors.New("scripted mode requires --code; --server and --name fall back to defaults if omitted, but the host name resolver returned empty — pass --name explicitly")
				}

				if err := doExchange(cmd.Context(), serverURL, code, name, title, authURL, out, clientOnly); err == nil {
					break
				} else {
					fmt.Fprintf(os.Stderr, "Registration failed: %v\n", err)
					if scripted {
						return err
					}
					again, perr := promptDefault(reader, "Try again? [Y/n]", "y")
					if perr != nil || !isYes(again) {
						return err
					}
					// Likely a bad/expired code — clear it so the next pass
					// asks again. Keep server + name; user probably wants
					// to keep those.
					code = ""
				}
			}

			if scripted {
				return nil
			}
			// Client-only registrations have no tunnel + no inbound
			// surface, so the only thing `outpost start` would do is
			// run the admin UI. Skip the auto-start prompt to avoid
			// implying this machine is now "online".
			if clientOnly {
				fmt.Println()
				fmt.Println("Client-only outpost paired. Use `outpost connect <host>` /")
				fmt.Println("`outpost ssh-proxy <host>` from your shell — nothing to start.")
				return nil
			}
			if !assumeYes {
				ans, _ := promptDefault(reader, "Start outpost now? [Y/n]", "y")
				if !isYes(ans) {
					fmt.Println()
					fmt.Println("To start later, run:")
					fmt.Println("    outpost start")
					return nil
				}
			}
			return execSelfStart()
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "Portal URL (default https://ai.dhnt.io)")
	cmd.Flags().StringVar(&code, "code", "", "One-time pairing code from the portal (skips the interactive prompt when set)")
	cmd.Flags().StringVar(&name, "name", "", "Host name to display in the portal (default: this machine's hostname)")
	cmd.Flags().StringVar(&out, "out", "", "Output config path (default: the OS-standard user-config path)")
	cmd.Flags().StringVar(&authURL, "auth-url", "",
		"Optional application-level auth endpoint. When set, the agent forwards {user,password} to it and trusts the returned role; the host OS is no longer consulted.")
	cmd.Flags().StringVar(&title, "title", "",
		"Human-readable subtitle shown in the portal (e.g. \"Family streaming box\"). Required when --auth-url is set; optional otherwise (falls back to the OS user / hostname).")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "On success, start outpost immediately without asking")
	cmd.Flags().BoolVar(&clientOnly, "client-only", false,
		"Pair this machine as a credential-only outpost — outbound SSH via `outpost ssh-proxy` only, no inbound listeners, no matrix tunnel. The host row shows up in cloudbox with a 'client' badge so the operator can see it; it cannot be a share target.")
	return cmd
}

// defaultHostName returns the system hostname with the macOS/mDNS
// `.local` and the older `.lan` suffix stripped, so a Mac that reports
// "dragon.local" pairs as just "dragon" (matching how users typically
// type the name interactively). Empty result is possible on rare
// systems where os.Hostname errors — callers are expected to surface
// that as "host name resolver returned empty, pass --name explicitly".
func defaultHostName() string {
	h, _ := os.Hostname()
	for _, sfx := range []string{".local", ".lan"} {
		if trimmed, ok := strings.CutSuffix(h, sfx); ok {
			return trimmed
		}
	}
	return h
}

// collectInputs prompts the user for the three required fields, using
// (in order of preference): an existing flag value, a sensible default,
// or — for the code — nothing (the prompt won't accept an empty value).
func collectInputs(r *bufio.Reader, serverURL, code, name *string) error {
	if *serverURL == "" {
		*serverURL = defaultPortal
	}
	got, err := promptDefault(r, "Portal", *serverURL)
	if err != nil {
		return err
	}
	*serverURL = got

	got, err = promptRequired(r, "Pairing code (paste from the portal)")
	if err != nil {
		return err
	}
	*code = got

	if *name == "" {
		*name = defaultHostName()
	}
	got, err = promptDefault(r, "Host name", *name)
	if err != nil {
		return err
	}
	*name = got
	return nil
}

// doExchange runs the pairing exchange and writes the resulting config to
// disk. Thin wrapper over portal.Exchange — the admin UI calls the same
// portal package directly so it can layer Apps + toggles in before saving.
func doExchange(ctx context.Context, serverURL, code, name, title, authURL, out string, clientOnly bool) error {
	fc, err := portal.Exchange(ctx, portal.ExchangeRequest{
		ServerURL:  serverURL,
		Code:       code,
		Name:       name,
		Title:      title,
		AuthURL:    authURL,
		ClientOnly: clientOnly,
	})
	if err != nil {
		return err
	}
	path := out
	if path == "" {
		p, perr := conf.DefaultConfigPath()
		if perr != nil {
			return perr
		}
		path = p
	}
	if err := conf.SaveFile(path, fc); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("Registered as %q. Config saved to %s\n", fc.AgentName, path)
	return nil
}

// promptDefault prints "label [def]: " and returns either the user's
// trimmed input or def. EOF on stdin (closed pipe, etc.) is surfaced as
// an error so scripted callers don't loop forever.
func promptDefault(r *bufio.Reader, label, def string) (string, error) {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read input: %w", err)
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// promptRequired loops until the user gives a non-empty value, or stdin
// closes (in which case we bail with an error rather than spin).
func promptRequired(r *bufio.Reader, label string) (string, error) {
	for {
		fmt.Printf("%s: ", label)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read input: %w", err)
		}
		s := strings.TrimSpace(line)
		if s != "" {
			return s, nil
		}
		fmt.Println("  (required — please paste the code from the portal)")
	}
}

func isYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "" || s == "y" || s == "yes"
}

// execSelfStart re-execs the current binary with "start" as a detached
// background process. The child writes logs to a cache-dir file and
// outlives the register command — closing the terminal won't kill it.
func execSelfStart() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	logPath, err := outpostLogPath()
	if err != nil {
		return fmt.Errorf("prepare log dir: %w", err)
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logF.Close()

	c := exec.Command(self, "start")
	// Background daemon: no controlling terminal, no stdin, log->file.
	c.Stdin = nil
	c.Stdout = logF
	c.Stderr = logF
	detach(c) // platform-specific: Setsid on unix, new process group on Windows.

	if err := c.Start(); err != nil {
		return fmt.Errorf("start outpost: %w", err)
	}
	pid := c.Process.Pid
	// Release so the child isn't reaped when register exits.
	_ = c.Process.Release()

	// Give the child a moment to crash-on-boot (bad config, port in use,
	// etc.) so we can surface that immediately instead of leaving the user
	// to discover it later.
	time.Sleep(500 * time.Millisecond)
	if !processAlive(pid) {
		tail, _ := os.ReadFile(logPath)
		fmt.Fprintln(os.Stderr, "outpost exited immediately. Tail of log:")
		fmt.Fprintln(os.Stderr, strings.TrimSpace(string(tail)))
		return fmt.Errorf("outpost did not stay running (logs: %s)", logPath)
	}

	fmt.Printf("Started outpost in background (pid %d).\n", pid)
	fmt.Printf("Logs: %s\n", logPath)
	fmt.Println("Stop with: outpost stop")
	return nil
}

// outpostLogPath returns ~/.cache/outpost/outpost.log on Linux+macOS
// (Windows: %USERPROFILE%\.cache\outpost\outpost.log), creating
// parent directories as needed.
func outpostLogPath() (string, error) {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "outpost.log"), nil
}

// processAlive returns true if a process with the given pid is currently
// running. Uses signal 0 — POSIX's "check whether you could deliver a
// signal" no-op. Good enough for a quick post-spawn liveness check.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// pidFilePath returns the path where startCmd records its pid so stopCmd
// (and the duplicate-instance check) can find it later.
func pidFilePath() (string, error) {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "outpost.pid"), nil
}

// claimPidFile writes the current process's pid to the pidfile. If a
// pidfile already exists and points at a live process, return an error
// (refuses to start a second instance). Stale pidfiles from a previous
// crash are silently overwritten.
func claimPidFile() error {
	p, err := pidFilePath()
	if err != nil {
		return err
	}
	if data, err := os.ReadFile(p); err == nil {
		if oldPid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && oldPid > 0 && processAlive(oldPid) {
			return fmt.Errorf("outpost is already running (pid %d). Stop it first with: outpost stop", oldPid)
		}
	}
	return os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// removePidFile clears the pidfile when startCmd exits normally. Best-
// effort — if the user nuked the file mid-run, we don't care.
func removePidFile() {
	if p, err := pidFilePath(); err == nil {
		_ = os.Remove(p)
	}
}

// stopCmd implements `outpost stop`. SIGTERMs the recorded pid, polls
// for up to 5 s, then escalates to SIGKILL.
func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop a backgrounded outpost",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := pidFilePath()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("No outpost is running.")
					return nil
				}
				return fmt.Errorf("read pid file: %w", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				_ = os.Remove(p)
				return fmt.Errorf("malformed pid file (removed): %w", err)
			}
			if !processAlive(pid) {
				_ = os.Remove(p)
				fmt.Println("Stale pid file removed — outpost was not running.")
				return nil
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find process %d: %w", pid, err)
			}
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("signal pid %d: %w", pid, err)
			}
			// Poll for graceful exit, up to 5 s. We treat EITHER
			// signal as "stopped cleanly":
			//   - processAlive returns false (process truly gone)
			//   - the pidfile disappeared (the daemon's deferred
			//     removePidFile ran, meaning main returned cleanly
			//     even if the process hasn't been reaped yet by
			//     its parent — relevant when outpost runs as a
			//     child of `go test` or similar, where it lingers
			//     as a zombie until the parent calls wait).
			for range 50 {
				if !processAlive(pid) {
					_ = os.Remove(p)
					fmt.Printf("Stopped outpost (pid %d).\n", pid)
					return nil
				}
				if _, err := os.Stat(p); os.IsNotExist(err) {
					fmt.Printf("Stopped outpost (pid %d).\n", pid)
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			// SIGTERM ignored — escalate.
			_ = proc.Signal(syscall.SIGKILL)
			time.Sleep(200 * time.Millisecond)
			_ = os.Remove(p)
			fmt.Printf("Force-killed outpost (pid %d) after SIGTERM timeout.\n", pid)
			return nil
		},
	}
}
