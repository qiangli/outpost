// Command outpost runs on a home host: it pairs with the portal and
// surfaces local apps (web, shell, desktop, clipboard) through a tunnel.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
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

	"github.com/filebrowser/filebrowser/v2/fbembed"
	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	actrunner "github.com/qiangli/coreutils/external/actrunner"
	kopia "github.com/qiangli/coreutils/external/kopia"
	seaweedfs "github.com/qiangli/coreutils/external/seaweedfs"
	zot "github.com/qiangli/coreutils/external/zot"
	"github.com/qiangli/coreutils/pkg/secrets"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/adminui"
	"github.com/qiangli/outpost/internal/agent/backup"
	"github.com/qiangli/outpost/internal/agent/certs"
	"github.com/qiangli/outpost/internal/agent/clusterllm"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/fleetreg"
	"github.com/qiangli/outpost/internal/agent/heartbeat"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/mcpapi"
	"github.com/qiangli/outpost/internal/agent/mesh"
	agentmirror "github.com/qiangli/outpost/internal/agent/mirror"
	"github.com/qiangli/outpost/internal/agent/ollama"
	"github.com/qiangli/outpost/internal/agent/otel"
	"github.com/qiangli/outpost/internal/agent/overlaykey"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
	"github.com/qiangli/outpost/internal/agent/peerimage"
	"github.com/qiangli/outpost/internal/agent/peerplane"
	"github.com/qiangli/outpost/internal/agent/portal"
	"github.com/qiangli/outpost/internal/agent/recipebuilder"
	"github.com/qiangli/outpost/internal/agent/repair"
	"github.com/qiangli/outpost/internal/agent/runtime"
	"github.com/qiangli/outpost/internal/agent/sandbox"
	"github.com/qiangli/outpost/internal/agent/selfcheck"
	"github.com/qiangli/outpost/internal/agent/shard"
	"github.com/qiangli/outpost/internal/agent/sysinfo"
	"github.com/qiangli/outpost/internal/agent/sysload"
	"github.com/qiangli/outpost/internal/agent/upgrade"
	"github.com/qiangli/outpost/internal/agent/userkube"
	"github.com/qiangli/outpost/internal/agent/vknode"
	"github.com/qiangli/outpost/internal/agent/warm"
	"github.com/qiangli/outpost/internal/scheduler"
	"github.com/qiangli/outpost/internal/telemetry"
	"k8s.io/client-go/rest"
)

// defaultPortal is the public ai.dhnt.io address used when the user
// doesn't override it via --server or the interactive prompt.
const defaultPortal = "https://ai.dhnt.io"

func main() {
	if code, handled := vknode.RunNativeProcessHelper(os.Args); handled {
		os.Exit(code)
	}
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
		// Don't dump the usage block on operational RunE errors — those
		// are runtime outcomes ("already at the latest release", "no
		// route to host"), not the operator mis-invoking the command.
		// The usage wall shadowed the real message. cobra still prints
		// the "Error: <msg>" line (SilenceErrors stays false), and flag/
		// arg help is one `--help` away.
		SilenceUsage: true,
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
		sshProxyCmd(), sshConfigCmd(), sshTreeCmd(), sshdCmd(), scpCmd(), shasumCmd(), reachCmd(), connectCmd(),
		outboundCmd(), jobsCmd(), fgCmd(), bgCmd(), killCmd(), runCmd(),
		clusterCmd(), departCmd(), poolCmd(), kubectlCmd(),
		// MCP-client CLI parity (Phase 1.5):
		appsCmd(), builtinsCmd(), configCmd(), statusCmd(), unpairCmd(), restartCmd(), mcpCmd(), tokenCmd(),
		remoteCmd(), meshCmd(), mirrorCmd(), shardCmd(), bundleCmd(), appstoreCmd(),
		docsCmd(), gitCmd(), shellCmd(), versionCmd(), upgradeCmd(), rollbackCmd(), buildCmd(), bashyCmd(),
		supervisordCmd(), serviceCmd(), doctorCmd(),
		// peerimage: peer image distribution verbs (recipes, not blobs).
		peerImageCmd(),
	)
	// Wave 3A: LAN peer discovery + peer-assisted repair. Registered
	// via a helper so main.go's root AddCommand block stays compact.
	registerDiscoveryCommands(root)
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
		Short: "Start the local agent — dials the portal when paired, otherwise serves only the admin UI and waits for pairing",
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

			// Layer-1 defense: opportunistically reassert this outpost's
			// identity with cloudbox before bringing up the tunnel. If
			// cloudbox lost the host row (deletion accident, stale
			// backup, migration mishap) the bearer-authed
			// /api/register/reattach endpoint recreates it from the
			// AccessToken we already have on disk. Best-effort —
			// network failures here do NOT block boot; the existing
			// pairing may still be perfectly intact and we don't want
			// a transient cloudbox blip to gate the daemon. The call
			// has its own 30s deadline; we use cmd.Context() here
			// because the errgroup ctx isn't built yet.
			fc, _ = tryReattach(cmd.Context(), fc, cfgPath)

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

			// Provision trusted-header HMAC keys before building the live app
			// registry. Registering first would leave this process with an empty
			// signing key even if the generated secret were persisted later.
			if minted, err := conf.EnsureAppSSOSecrets(cfgPath, fc); err != nil {
				return fmt.Errorf("app sso secrets: %w", err)
			} else if len(minted) > 0 {
				slog.Warn("auto-generated sso_secret for trust_cloud_identity apps — paste the new value into each upstream app's config via `outpost apps secret <name>`",
					"apps", strings.Join(minted, ","))
			}
			if minted, err := conf.EnsureBashyServiceSSOSecrets(cfgPath, fc, effectiveBashyServices(fc)); err != nil {
				return fmt.Errorf("bashy service sso secrets: %w", err)
			} else if len(minted) > 0 {
				slog.Info("auto-generated sso_secret for trusted bashy services",
					"services", strings.Join(minted, ","))
			}

			apps, err := buildAppRegistry(fc, cfg.Apps)
			if err != nil {
				return err
			}
			if err := registerBashyServiceApps(fc, apps); err != nil {
				return err
			}
			// Built-in local-daemon proxies (podman, ollama, otel below).
			// These are default-ON (see conf.PodmanOn) and DETECTION-GATED:
			// we skip registration when the daemon isn't actually reachable,
			// so a host that has never installed podman/Ollama boots exactly
			// as before — one log line, no error. o3 is an offer, never a
			// dependency.
			//
			// The skip is logged at Info when the default put us here and at
			// Warn only when the operator explicitly asked for the proxy: a
			// laptop with no podman is normal, a config that says
			// podman_enabled=true with no daemon is a real misconfiguration.
			missingBackend := func(explicit bool, msg string, args ...any) {
				if explicit {
					slog.Warn(msg, args...)
					return
				}
				slog.Info(msg, args...)
			}
			if fc.PodmanOn() {
				if bt := agent.DetectPodman(); bt.Available && bt.Socket != "" {
					if err := apps.RegisterFromConfig(conf.AppConfig{
						// Scheme comes from the detector, never a literal: on
						// Windows podman's endpoint is a named pipe, so a
						// hardcoded "unix" would register an unreachable app.
						Name: agent.BuiltinPodman, Scheme: bt.Scheme, Socket: bt.Socket,
						// RequireLogin MUST be set explicitly: the raw podman
						// socket is root-equivalent. Role:"admin" alone does
						// NOT gate it — Role→RequireLogin only happens in
						// NewFromJSON's migrateLegacyRole, which this in-code
						// AppConfig bypasses, so it was reaching cloudbox's
						// anonymous (require_login=false) proxy path.
						Role: "admin", RequireLogin: true, Enabled: true,
					}); err != nil {
						slog.Warn("podman builtin: register", "err", err)
					} else {
						slog.Info("podman builtin: registered", "socket", bt.Socket)
					}
				} else {
					missingBackend(fc.PodmanEnabled != nil, "podman builtin: no podman socket detected — skipping (outpost continues without it)")
				}
			}
			// Filtered container "sandbox" proxy. Shares the podman socket
			// with the raw passthrough above but registers a SEPARATE app
			// whose proxy is wrapped by the sandbox filter (strips
			// privileged / host namespaces / host binds / added caps /
			// devices, injects resource caps). This is the mount a thin
			// client or an untrusted tenant talks to; the raw /app/podman/
			// stays admin-only for trusted self-use. Decorated like the
			// ollama mount: capability advertisement (so cloudbox can
			// discover + pool sandbox hosts) + capacity intercept + the
			// filter/counter proxy wrap. The prewarmer (started under the
			// errgroup below) keeps the runnable images pulled.
			var sbPrewarmer *sandbox.Prewarmer
			if fc.SandboxOn() {
				if bt := agent.DetectPodman(); bt.Available && bt.Socket != "" {
					if err := apps.RegisterFromConfig(conf.AppConfig{
						Name: agent.BuiltinSandbox, Scheme: bt.Scheme, Socket: bt.Socket,
						RequireLogin: true, Enabled: true,
					}); err != nil {
						slog.Warn("sandbox builtin: register", "err", err)
					} else {
						slog.Info("sandbox builtin: registered", "socket", bt.Socket)
						policy := sandbox.Policy{
							MaxMemoryBytes:    fc.SandboxMaxMemoryMB * 1024 * 1024,
							NanoCPUs:          int64(fc.SandboxCPUs * 1e9),
							PidsLimit:         fc.SandboxPidsLimit,
							MaxContainers:     fc.SandboxMaxContainers,
							AllowedImages:     fc.SandboxAllowedImages,
							ScratchHostPrefix: fc.SandboxScratchDir,
						}
						sbSvc := sandbox.NewService(policy)
						apps.SetCapabilities(agent.BuiltinSandbox, &agent.AppCapabilities{Type: sandbox.CapabilityType})
						apps.SetProxyWrap(agent.BuiltinSandbox, sbSvc.WrapProxy)
						apps.AddIntercept(agent.BuiltinSandbox, "/_pool/capacity", sbSvc.CapacityHandler())
						// Image prewarmer: pull the runnable images so a
						// remote create+start skips the pull cost. Default
						// to the allowlist when no explicit prewarm list is
						// set — pre-pulling exactly what callers may run.
						if imgs := sandboxPrewarmImages(fc); len(imgs) > 0 {
							sbPrewarmer = sandbox.NewPrewarmer(bt.Socket, imgs)
							sbSvc.SetPrewarmer(sbPrewarmer)
						}
					}
				} else {
					slog.Warn("sandbox builtin enabled but podman daemon not detected — skipping")
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
			// clusterDetector is the (cached) probe for an intra-home
			// distributed-inference backend (GPUStack first). Constructed
			// once when an endpoint is configured and shared by the
			// registry-push watcher (advertises the cluster to cloudbox)
			// and the admincore SafeView (renders it in the admin UI /
			// `outpost status`). Nil when ClusterLLMEndpoint is empty —
			// every single-machine outpost — so nothing probes.
			var clusterDetector *clusterllm.Detector
			if fc.ClusterLLMOn() {
				clusterDetector = clusterllm.NewDetector(clusterllm.Config{
					Endpoint: fc.ClusterLLMEndpoint,
					APIKey:   fc.ClusterLLMAPIKey,
				}, 0, nil)
			}
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
					missingBackend(fc.OllamaEnabled != nil, "ollama builtin: no Ollama daemon detected — skipping (outpost continues without it)")
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
					missingBackend(fc.OtelEnabled != nil, "otel builtin: no local observability proxy detected — skipping (outpost continues without it)", "manifest", t.ManifestPath)
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

			// Peer-plane locality service (opt-in, OFF by default). Created
			// here so its measured-tier snapshot can render into SafeView +
			// the outpost_peer_tiers MCP tool; started in the errgroup below.
			// Self-disables when unpaired.
			var peerPlaneSvc *peerplane.Service
			if fc.PeerPlaneNeeded() && fc.AccessToken != "" {
				if cb := cloudboxHTTPBase(fc); cb != "" {
					peerPlaneSvc = peerplane.New(peerplane.Config{
						AgentName:   cfg.AgentName,
						CloudboxURL: cb,
						AccessToken: fc.AccessToken,
					})
				}
			}
			peerTiers := func() []admincore.PeerTierView {
				if peerPlaneSvc == nil {
					return nil
				}
				snap := peerPlaneSvc.Snapshot()
				out := make([]admincore.PeerTierView, 0, len(snap))
				for _, t := range snap {
					out = append(out, admincore.PeerTierView{
						Host: t.Host, Tier: string(t.Tier), RTTms: t.RTT, Addr: t.Addr,
						EgressSameLANHint: t.SameLANHint, At: t.At,
					})
				}
				return out
			}

			// libp2p mesh data plane (opt-in, OFF by default) — the peer
			// node carrying authenticated, NAT-traversing peer↔peer streams
			// (the transport under shard-RPC, peer-backup, the resource
			// fabric). Created here so its status renders into SafeView;
			// started in the errgroup below. Self-disables when unpaired
			// (cloudbox is the rendezvous that finds peers).
			// Fetch cloudbox's circuit-relay multiaddrs (best-effort) so the
			// mesh host runs AutoRelay against them — strict-NAT DCUtR. Absent
			// relay (or cloudbox unreachable at boot) just means no relay leg;
			// same-LAN/same-vicinity still connects directly.
			var meshRelays []string
			if fc.MeshNeeded() && fc.AccessToken != "" {
				if cb := cloudboxHTTPBase(fc); cb != "" {
					rc := &peerplane.Client{BaseURL: cb, Token: fc.AccessToken, HC: &http.Client{Timeout: 10 * time.Second}}
					rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
					if rs, rerr := rc.Relays(rctx); rerr == nil {
						meshRelays = rs
					}
					rcancel()
				}
			}
			var meshHost *mesh.Host
			if fc.MeshNeeded() && fc.AccessToken != "" {
				mh, merr := mesh.New(mesh.Config{
					AgentName:  fc.AgentName,
					ListenPort: fc.MeshPort,
					RelayAddrs: meshRelays,
					Logger:     slog.Default(),
				})
				if merr != nil {
					// Non-fatal: the mesh is best-effort peer connectivity and is
					// now default-on fleet-wide — a libp2p start failure must NEVER
					// take down the daemon (that would strand a remote host). Log
					// and continue without the mesh data plane.
					slog.Warn("mesh: host failed to start; continuing without mesh data plane", "err", merr)
				} else {
					meshHost = mh
				}
			}
			meshStatus := func() *admincore.MeshStatusView {
				if meshHost == nil {
					return nil
				}
				s := meshHost.Status()
				peers := make([]admincore.MeshPeerConnView, 0, len(s.Peers))
				for _, p := range s.Peers {
					peers = append(peers, admincore.MeshPeerConnView{
						ID:        p.ID,
						Direct:    p.Direct,
						LinkClass: p.LinkClass,
						Remote:    p.Remote,
					})
				}
				return &admincore.MeshStatusView{
					PeerID:         s.PeerID,
					ListenAddrs:    s.ListenAddrs,
					ConnectedPeers: s.ConnectedPeers,
					Peers:          peers,
				}
			}
			// Mesh rendezvous client — uses cloudbox's peer-signal surface to
			// announce this host + discover/dial paired peers. Started in the
			// errgroup below.
			var meshRdv *mesh.Rendezvous
			if meshHost != nil {
				if cb := cloudboxHTTPBase(fc); cb != "" {
					meshRdv = mesh.NewRendezvous(meshHost, fc.AgentName, cb, fc.AccessToken, slog.Default())
				}
			}
			// The peer-plane prober and the mesh rendezvous both announce to
			// cloudbox, and cloudbox keeps ONE PeerNode row per host whose
			// peer_id + candidates are overwritten wholesale on every announce.
			// Left independent, the prober's blank peer id erased the mesh
			// identity every 60s and `/api/v1/peer/resolve` (which skips rows
			// without a peer id) dropped this host's mesh services — `mesh dial
			// ssh` then flapped between "resolved" and "no reachable peer
			// exposes service". Cross-wire the two so each publishes the same
			// composite (mesh peer id + union of candidates).
			if peerPlaneSvc != nil && meshHost != nil {
				peerPlaneSvc.SetIdentity(func() (string, []string) {
					return meshHost.PeerID(), meshHost.DialableAddrs()
				})
				if meshRdv != nil {
					meshRdv.SetExtraCandidates(peerPlaneSvc.Candidates)
				}
			}
			// Live mesh link class + LAN label per paired host — the accurate
			// same-LAN signal (enriched with WHICH lan the direct link rides
			// over) that overrides cloudbox's egress-IP location heuristic in
			// admincore.PeerStatus. nil-safe: a zero MeshLinkInfo leaves the
			// cloudbox hint untouched.
			var meshLinkInfoByHost func(host string) admincore.MeshLinkInfo
			if meshRdv != nil {
				meshLinkInfoByHost = func(host string) admincore.MeshLinkInfo {
					li := meshRdv.LinkInfoForHost(host)
					return admincore.MeshLinkInfo{Class: li.Class, LAN: li.LAN}
				}
			}
			// Host -> peer id the mesh actually connected with. Feeds
			// admincore.MeshHostLink, which lets `outpost reach` prefer an
			// already-established direct peer link over the cloudbox relay
			// without re-deriving locality through mDNS.
			var meshPeerIDByHost func(host string) string
			if meshRdv != nil {
				meshPeerIDByHost = meshRdv.PeerIDForHost
			}
			// Adapter so admincore can drive the forwarder (expose/listen)
			// without importing the mesh package.
			var meshFwd admincore.MeshForwardOps
			if meshHost != nil {
				meshFwd = meshFwdAdapter{f: meshHost.Forwarder()}
				// Wrap harness: auto-expose the persistently-configured mesh
				// services so they survive restarts (declarative `mesh expose`).
				for _, s := range fc.MeshServices {
					if s.Name != "" && s.Addr != "" {
						meshHost.Forwarder().Expose(s.Name, s.Addr)
					}
				}
				// Symmetric consume side: re-establish the persistent mesh
				// forwards (dial by peer id, no cloudbox resolve) so a cross-host
				// dependency — e.g. this node's act_runner reaching a loom forge on
				// another host — is up at boot, BEFORE the actrunner block below.
				// The listener binds immediately; the per-connection stream to the
				// peer connects lazily once the mesh links up (act_runner retries).
				for _, c := range fc.MeshConsumes {
					if c.Service == "" || c.PeerID == "" {
						continue
					}
					// NOTE: the raw Forwarder.Listen arg order is (localAddr,
					// peerID, service) — NOT admincore's (peerID, service,
					// localAddr). Passing them in the wrong order binds on the
					// peer-id string and silently no-ops the consume at boot
					// (the v0.13.3 bug). The listener binds immediately; the
					// per-connection stream connects lazily once the mesh links up.
					if lis, cerr := meshHost.Forwarder().Listen(c.LocalAddr, c.PeerID, c.Service); cerr != nil {
						slog.Warn("mesh consume: failed to establish forward", "service", c.Service, "peer", c.PeerID, "err", cerr)
					} else {
						slog.Info("mesh consume: forwarding", "service", c.Service, "peer", c.PeerID, "local", lis.Addr().String())
					}
				}
			}
			// Service registry resolver: query cloudbox for the peers exposing a
			// named mesh service (the zero-config consume side).
			var meshResolver func(service string) ([]admincore.MeshResolvedPeer, error)
			if meshHost != nil {
				if cb := cloudboxHTTPBase(fc); cb != "" {
					rcli := &peerplane.Client{BaseURL: cb, Token: fc.AccessToken, HC: &http.Client{Timeout: 10 * time.Second}}
					meshResolver = func(service string) ([]admincore.MeshResolvedPeer, error) {
						rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						peers, err := rcli.Resolve(rctx, service)
						if err != nil {
							return nil, err
						}
						out := make([]admincore.MeshResolvedPeer, 0, len(peers))
						for _, p := range peers {
							out = append(out, admincore.MeshResolvedPeer{Host: p.Host, PeerID: p.PeerID, Services: p.Services})
						}
						return out, nil
					}
				}
			}

			// Shard manager — keeps a launch-ready candidate ring up to date
			// and (over the mesh) tells peers to lead / reports readiness.
			// Constructed here (not at its errgroup start below) so the
			// admincore shard closures can capture it. nil when sharding /
			// mesh / pairing isn't all on. Started in the errgroup further down.
			shardMgr := newShardManager(fc, meshHost, peerPlaneSvc, meshRdv)

			// resolveShardPeer maps a host name to its mesh ShardPeer via the
			// cloudbox peer/connect rendezvous (mirrors peerPlaneDiscoverer).
			resolveShardPeer := func(ctx context.Context, host string) (shard.ShardPeer, error) {
				cb := cloudboxHTTPBase(fc)
				if cb == "" {
					return shard.ShardPeer{}, fmt.Errorf("not paired: no cloudbox base to resolve peer %q", host)
				}
				client := &peerplane.Client{BaseURL: cb, Token: fc.AccessToken, HC: &http.Client{Timeout: 10 * time.Second}}
				target, err := client.Connect(ctx, fc.AgentName, host)
				if err != nil {
					return shard.ShardPeer{}, err
				}
				if target == nil || target.Peer.PeerID == "" {
					return shard.ShardPeer{}, fmt.Errorf("peer %q has no resolvable mesh peer id", host)
				}
				return shard.ShardPeer{Host: host, PeerID: target.Peer.PeerID}, nil
			}
			// shardTrigger tells <host> to LEAD a shard for <model> over the mesh.
			shardTrigger := func(ctx context.Context, host, model string) error {
				if shardMgr == nil {
					return fmt.Errorf("sharding not enabled on this host (needs pairing + mesh + sharding on)")
				}
				if host == "" || host == "self" || host == "local" {
					// Lead the shard from THIS node, using its own ring — no mesh
					// self-dial. Long-running (self-provision pulls the model), so
					// background it on a detached context and return immediately.
					go func() {
						if err := shardMgr.Orchestrate(context.Background(), model, 11434, nil); err != nil {
							slog.Warn("shard: local lead failed", "model", model, "err", err)
						}
					}()
					return nil
				}
				peer, err := resolveShardPeer(ctx, host)
				if err != nil {
					return err
				}
				return shardMgr.TellLead(ctx, peer, model, 11434)
			}
			// shardStatus returns the local node's (host=="") or a peer's readiness.
			shardStatus := func(ctx context.Context, host string) (any, error) {
				if shardMgr == nil {
					return nil, fmt.Errorf("sharding not enabled on this host (needs pairing + mesh + sharding on)")
				}
				if host == "" {
					return shardMgr.LocalStatus(), nil
				}
				peer, err := resolveShardPeer(ctx, host)
				if err != nil {
					return nil, err
				}
				return shardMgr.PingPeer(ctx, peer)
			}
			// shardLog returns the local node's (host=="") or a peer's prima logs.
			shardLog := func(ctx context.Context, host string) (string, error) {
				if shardMgr == nil {
					return "", fmt.Errorf("sharding not enabled on this host (needs pairing + mesh + sharding on)")
				}
				if host == "" {
					return shardMgr.RecentPrimaLogs(200), nil
				}
				peer, err := resolveShardPeer(ctx, host)
				if err != nil {
					return "", err
				}
				return shardMgr.PeerLog(ctx, peer)
			}

			// Construct the shared business-logic layer first. The same
			// admincore.Server instance feeds adminui (human SPA) and
			// — soon — mcpapi (agent tools), so the file-save mutex and
			// restart-debounce timer are shared across the two surfaces.
			// Bind the control-plane status probe. Read cluster config + join endpoint once
			// so the probe closures see the CURRENT config snapshot, not a future mutation.
			cc := fc.Cluster
			controlPlaneEnabled := func() bool { return cc != nil && cc.ControlPlaneOn() }
			joinEndpoint := func() string {
				if cc == nil || !cc.ControlPlaneOn() {
					return ""
				}
				cpAddr, cpPort := cc.TunnelBind()
				return net.JoinHostPort(cpAddr, strconv.Itoa(cpPort))
			}
			nodes := func(ctx context.Context) ([]admincore.Node, error) {
				if cc == nil {
					return []admincore.Node{}, nil
				}
				nodeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return admincore.ReadControlPlaneNodes(nodeCtx, cc.ControlPlaneKubeconfig)
			}
			hasJoinToken := func() bool { return hasHostedJoinToken(cc) }
			hasNodeToken := func() bool { return cc != nil && strings.TrimSpace(cc.NodeToken) != "" }
			hasSTCPSecret := func() bool { return cc != nil && strings.TrimSpace(cc.STCPSecret) != "" }

			// peerimage: peer image distribution (recipes, not blobs) — the
			// peer half of docs/peer-dks-image-distribution.md. Constructed
			// here so the SAME *peerimage.Service feeds admincore (and through
			// it the MCP tools + CLI subcommands); serving its recipe index
			// over the mesh happens in the errgroup below. Gated on the
			// peer_image toggle + an agent runtime (the <node>-runtime
			// container is what ensure/report read and build into). bashy is
			// resolved lazily (NewBashyRunner / CtrRuntime.BashyPath), so
			// construction never blocks boot on provisioning the toolchain.
			var peerImageSvc *peerimage.Service
			if fc.PeerImageOn() && fc.ClusterOn() && fc.Cluster.HasAgentRuntime() && fc.ClusterNodeName() != "" {
				if cacheDir, cderr := conf.ResolveCacheDir(); cderr != nil || cacheDir == "" {
					slog.Warn("peerimage: no cache dir; peer image distribution disabled", "err", cderr)
				} else if store, serr := peerimage.NewStore(filepath.Join(cacheDir, "peerimage")); serr != nil {
					slog.Warn("peerimage: recipe store init failed; peer image distribution disabled", "err", serr)
				} else {
					nodeName := fc.ClusterNodeName()
					runtimeContainer := nodeName + "-runtime"
					peerImageSvc = &peerimage.Service{
						Identity: peerimage.NodeIdentity{Name: nodeName},
						Store:    store,
						Runtime: peerimage.CtrRuntime{
							BashyPath: bashyResolver.Path,
							Container: runtimeContainer,
						},
						Build: peerimage.RecipeBuilder{
							Runner:           recipebuilder.NewBashyRunner(bashyResolver.Path),
							RuntimeContainer: runtimeContainer,
						},
						Resolver: func(service string) ([]peerimage.PeerRef, error) {
							if meshResolver == nil {
								return nil, fmt.Errorf("mesh service registry is not available (pair the host and enable the mesh)")
							}
							peers, err := meshResolver(service)
							if err != nil {
								return nil, err
							}
							out := make([]peerimage.PeerRef, 0, len(peers))
							for _, p := range peers {
								out = append(out, peerimage.PeerRef{Host: p.Host, PeerID: p.PeerID, Services: p.Services})
							}
							return out, nil
						},
					}
					slog.Info("peerimage: peer image distribution enabled", "node", nodeName, "service", fc.PeerImageServiceOrDefault())
				}
			}

			core, err := admincore.New(admincore.Deps{
				ConfigPath:          cfgPath,
				Apps:                apps,
				Outbound:            outbound,
				Restart:             restartFn,
				CloudboxBase:        cloudboxHTTPBase(fc),
				CloudboxAccessToken: fc.AccessToken,
				AgentName:           fc.AgentName,
				LLMPoolStatus:       llmPoolStatus,
				PeerTiers:           peerTiers,
				MeshStatus:          meshStatus,
				MeshForward:         meshFwd,
				MeshResolver:        meshResolver,
				MeshLinkInfoByHost:  meshLinkInfoByHost,
				MeshPeerIDByHost:    meshPeerIDByHost,
				ShardTrigger:        shardTrigger,
				ShardStatus:         shardStatus,
				ShardLog:            shardLog,
				ControlPlaneStatusProber: admincore.NewDefaultControlPlaneStatusProber(
					fc.AgentName,
					controlPlaneEnabled,
					joinEndpoint,
					nodes,
					hasJoinToken,
					hasNodeToken,
					hasSTCPSecret,
				),
				ClusterRuntimeDown: func(ctx context.Context, purge bool) error {
					// Resolve the node name from the CURRENT config (LeaveCluster
					// just rewrote it, preserving Mode + NodeName). Stop the runtime
					// container and, on purge, drop its identity volumes so a rejoin
					// registers fresh — see runtime.PurgeVolumes.
					fc2, lerr := conf.LoadFile(cfgPath)
					if lerr != nil || fc2 == nil {
						return lerr
					}
					nn := fc2.ClusterNodeName()
					if nn == "" {
						return nil
					}
					opts := runtime.Options{AgentName: nn}
					if derr := runtime.Down(ctx, opts); derr != nil {
						return derr
					}
					if purge {
						return runtime.PurgeVolumes(ctx, opts)
					}
					return nil
				},
				// peerimage: nil when the feature is off — the verbs then
				// report "not enabled" through every surface.
				PeerImage: peerImageSvc,
			})
			if err != nil {
				return fmt.Errorf("admincore: %w", err)
			}

			// Considerate warm-serving plane (P3). The sysload profiler
			// samples CPU/memory/load, learns a per-hour-of-day baseline,
			// and answers Busy() / WarmBudgetBytes(); the warm executor
			// keeps a small conservative set of models resident (zero
			// cold-start) but YIELDS to the user's own work the moment the
			// host is busy, restoring when idle. Default ON for a paired
			// Ollama node (WarmServingOn); opt out with
			// warm_serving_enabled=false. Wired below into the watcher push
			// (WarmBudgetBytes/Busy advertisement) and the /admin/warm
			// route, and started in the errgroup.
			var (
				loadProfiler *sysload.Profiler
				warmExec     *warm.Executor
				hostMemTotal uint64
			)
			if fc.WarmServingOn() || (fc.ClusterOn() && len(fc.Cluster.VirtualRuntimes()) > 0) {
				var sysloadPath string
				if cd, _ := conf.ResolveCacheDir(); cd != "" {
					sysloadPath = filepath.Join(cd, "sysload.json")
				}
				loadProfiler = sysload.New(sysload.Config{
					Path: sysloadPath,
					Frac: fc.WarmBudgetFracOrDefault(),
				})
			}
			if fc.WarmServingOn() {
				hostMemTotal = sysinfo.Collect("").MemTotalBytes
				// The executor drives the local Ollama daemon (+ the shard
				// manager, when wired) and persists the desired warm set
				// through admincore. Only on a paired host — cloudbox is what
				// asks a host to warm a model.
				if fc.AccessToken != "" {
					warmOllamaURL := ollamaURL
					if warmOllamaURL == "" {
						warmOllamaURL = "http://127.0.0.1:11434"
					}
					var shardCtl warm.ShardControl
					if shardMgr != nil {
						shardCtl = shardMgr // avoid a non-nil interface wrapping a nil *Manager
					}
					warmExec = warm.New(warm.Config{
						Ollama:         warm.NewOllamaClient(warmOllamaURL),
						Shard:          shardCtl,
						Gauge:          loadProfiler,
						UsableMem:      func() uint64 { return hostMemTotal },
						Desired:        fc.WarmDesired,
						PersistDesired: core.SetWarmDesired,
					})
				}
			}

			// Cloudbox-pushed CI-repair trigger (POST /admin/repair). Only on
			// a paired host — the route is tunnel-fronted and cloudbox is what
			// pushes a repair. The repair program is configured via
			// OUTPOST_REPAIR_CMD (argv, space-split); empty mounts the route
			// but replies 503 until set, so the schedule-cron pull backstop
			// stays the reliable path. See the umbrella auto-fix pipeline doc.
			var repairExec *repair.Executor
			if fc.AccessToken != "" {
				repairExec = repair.New(repair.Config{
					Command: strings.Fields(os.Getenv("OUTPOST_REPAIR_CMD")),
				})
			}

			// Persisted boot counter, reported to cloudbox each /apps
			// poll so the fleet health-gate can detect a crash-loop
			// (boot_count jumping > 1 inside a rollout bake window).
			// Unconditional — crash-loop detection matters for every
			// daemon, paired or not.
			if bcDir, _ := conf.ResolveCacheDir(); bcDir != "" {
				agent.InitBootCount(bcDir)
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
				upgradeWorker      *upgrade.Worker
				upgradeLedger      *upgrade.Ledger
				upgradeConfirmPath string
			)
			if fc.AccessToken != "" {
				cacheDir, _ := conf.ResolveCacheDir()
				upgradeConfirmPath = upgrade.PendingConfirmPath(cacheDir)
				// Report "healthy=false" to cloudbox while a self-upgrade
				// is pending confirmation (the watchdog marker is present),
				// so the fleet health-gate sees an unconfirmed host. Hooked
				// (not a direct call) to avoid an agent→upgrade import cycle.
				if cp := upgradeConfirmPath; cp != "" {
					agent.HealthyProbe = func() bool {
						pc, _ := upgrade.ReadPendingConfirm(cp)
						return pc == nil
					}
				}
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
							CurrentCommit: agent.ReadBuildInfo().ShortCommit(),
							CurrentDirty:  agent.ReadBuildInfo().Dirty,
							BinaryPath:    exe,
							PendingPath:   pendingPath,
						}
					},
					Restart:        core.ScheduleRestart,
					Ledger:         upgradeLedger,
					ConfirmPath:    upgradeConfirmPath,
					QuarantinePath: upgrade.QuarantinePath(cacheDir),
				})
				if err != nil {
					return fmt.Errorf("upgrade worker: %w", err)
				}
				// Make the worker + ledger visible to admincore's
				// shared business-logic layer so the adminui Update
				// tab + MCP tools all read from the same source.
				core.AttachUpgrade(upgradeWorker, upgradeLedger)
			}

			// Folder-watcher backup scheduler. Constructed regardless
			// of pairing — the cooperating app can start producing
			// artifacts before the outpost is paired (Phase 2 just
			// records candidates; Phase 3 adds the peer-push that
			// requires cloudbox). One process-wide scheduler + one
			// process-wide manager so manual "Run now" from the SPA
			// and the scheduled fire never overlap.
			backupSched := scheduler.New(filepath.Join(func() string {
				cache, _ := conf.ResolveCacheDir()
				return cache
			}(), "scheduler.log"))
			backupMgr := backup.NewManager(backupSched, backup.DefaultLedgerPath())
			if err := backupMgr.Apply(fc.Backup); err != nil {
				slog.Warn("backup: initial apply failed", "err", err)
			}
			// Pusher pushes age-encrypted artifacts to cloudbox after
			// each worker fire. No-op on unpaired hosts (Configured()
			// returns false), so attaching is unconditional.
			backupMgr.AttachPusher(backup.NewPusher(backup.PushConfig{
				CloudboxBase: cloudboxHTTPBase(fc),
				AccessToken:  fc.AccessToken,
				AgentName:    fc.AgentName,
			}))
			core.AttachBackup(backupMgr)

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
				// Wave 3B.2: pluggable discovery-cache snapshot. The
				// closure reads `daemonCache` lazily; it's nil-safe
				// because the var is package-scoped and stays nil
				// when discovery is off.
				PeersFn: func() any {
					if daemonCache == nil {
						return map[string]any{"peers": []any{}}
					}
					return daemonCache.Snapshot()
				},
				GossipMembersFn: func() any {
					if daemonGossip == nil {
						return []any{}
					}
					return daemonGossip.Members()
				},
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

			startBashyServiceSupervisors(g, gctx, fc, meshHost)

			// Zot OCI registry (wrap-harness tool lifecycle): run Zot as a
			// managed external binary on a loopback port + auto-expose it over the
			// mesh as "registry" (serves container images + Ollama models). Zot is
			// NOT compiled into outpost. Non-fatal: a fetch/launch failure logs.
			if fc.ZotOn() {
				zotPort := fc.ZotPortOrDefault()
				zotData := "zot"
				if cd, _ := conf.ResolveCacheDir(); cd != "" {
					zotData = filepath.Join(cd, "zot")
				}
				g.Go(func() error {
					inst, zerr := zot.Start(gctx, zot.Options{
						Addr: "127.0.0.1", Port: zotPort, DataDir: zotData,
						Stdout: io.Discard, Stderr: io.Discard,
					})
					if zerr != nil {
						slog.Warn("zot registry: not started", "err", zerr)
						return nil
					}
					slog.Info("zot registry serving", "url", inst.URL, "zot", inst.Version)
					if meshHost != nil {
						meshHost.Forwarder().Expose("registry", inst.Addr)
						slog.Info("zot: exposed over the mesh as 'registry'", "addr", inst.Addr)
					}
					<-gctx.Done()
					return inst.Stop()
				})
			}

			// SeaweedFS object/blob store (wrap-harness tool lifecycle): run its
			// S3 gateway on a loopback port + auto-expose over the mesh as "s3".
			// SeaweedFS is NOT compiled into outpost. Non-fatal on fetch/launch.
			if fc.SeaweedfsOn() {
				swPort := fc.SeaweedfsPortOrDefault()
				swData := "seaweedfs"
				if cd, _ := conf.ResolveCacheDir(); cd != "" {
					swData = filepath.Join(cd, "seaweedfs")
				}
				g.Go(func() error {
					inst, serr := seaweedfs.Start(gctx, seaweedfs.Options{
						Addr: "127.0.0.1", Port: swPort, DataDir: swData,
						Stdout: io.Discard, Stderr: io.Discard,
					})
					if serr != nil {
						slog.Warn("seaweedfs: not started", "err", serr)
						return nil
					}
					slog.Info("seaweedfs S3 gateway serving", "url", inst.URL, "seaweedfs", inst.Version)
					if meshHost != nil {
						meshHost.Forwarder().Expose("s3", inst.Addr)
						slog.Info("seaweedfs: exposed over the mesh as 's3'", "addr", inst.Addr)
					}
					<-gctx.Done()
					return inst.Stop()
				})
			}

			// Kopia snapshot-backup repository server (wrap-harness tool
			// lifecycle): run it on a loopback port + auto-expose over the mesh as
			// "backup" (many nodes back up into one repo). Kopia is NOT compiled
			// into outpost. Non-fatal on fetch/launch error.
			if fc.KopiaOn() {
				kPort := fc.KopiaPortOrDefault()
				kData := "kopia"
				if cd, _ := conf.ResolveCacheDir(); cd != "" {
					kData = filepath.Join(cd, "kopia")
				}
				g.Go(func() error {
					inst, kerr := kopia.Start(gctx, kopia.Options{
						Addr: "127.0.0.1", Port: kPort, DataDir: kData,
						Stdout: io.Discard, Stderr: io.Discard,
					})
					if kerr != nil {
						slog.Warn("kopia backup: not started", "err", kerr)
						return nil
					}
					slog.Info("kopia backup server serving", "url", inst.URL, "kopia", inst.Version)
					if meshHost != nil {
						meshHost.Forwarder().Expose("backup", inst.Addr)
						slog.Info("kopia: exposed over the mesh as 'backup'", "addr", inst.Addr)
					}
					<-gctx.Done()
					return inst.Stop()
				})
			}

			// Gitea act_runner (CI executor — wrap-harness tool lifecycle, but a
			// CONSUMER not a mesh service): registers against a Gitea instance and
			// dials OUT to run .gitea/workflows/*.yml, so it's NAT/mesh-friendly.
			// act_runner is NOT compiled into outpost (binmgr-managed external).
			// Non-fatal on error. See docs/local-p2p-cicd.md.
			if fc.ActrunnerOn() {
				arData := "act_runner"
				if cd, _ := conf.ResolveCacheDir(); cd != "" {
					arData = filepath.Join(cd, "act_runner")
				}
				instance := fc.ActrunnerInstanceResolved()
				labels := fc.ActrunnerLabelsOrDefault()
				token := fc.ActrunnerToken
				g.Go(func() error {
					if instance == "" {
						slog.Warn("act_runner: no instance — set actrunner_instance or enable loom; skipping")
						return nil
					}
					// Supervise register + daemon with backoff. A build-only node
					// reaches loom over a mesh forward that may not be up at boot
					// (see mesh_consumes), so the first register/poll can fail —
					// retry instead of giving up forever (the pre-fix behavior
					// stranded the runner until the next daemon restart).
					backoff := 5 * time.Second
					const maxBackoff = 2 * time.Minute
					for gctx.Err() == nil {
						if !actrunner.Registered(arData) {
							if token == "" {
								slog.Warn("act_runner: not registered and no actrunner_token set; skipping")
								return nil
							}
							if rerr := actrunner.Register(gctx, actrunner.RegisterOptions{
								DataDir: arData, Instance: instance, Token: token, Labels: labels,
								Sandbox: fc.ActrunnerSandboxOn(), SandboxImage: fc.ActrunnerSandboxImage,
							}); rerr != nil {
								slog.Warn("act_runner: registration failed; retrying", "err", rerr, "backoff", backoff)
								if !sleepCtx(gctx, backoff) {
									return nil
								}
								backoff = minDur(backoff*2, maxBackoff)
								continue
							}
							slog.Info("act_runner: registered", "instance", instance, "labels", labels)
							backoff = 5 * time.Second
						}
						daemonEnv := telemetry.ChildEnv("act_runner")
						if fc.ActrunnerSandboxOn() {
							// Ensure the sandbox config exists even if the runner was
							// registered before sandbox mode was enabled (Register
							// writes it, but that path is skipped once .runner exists).
							if cerr := actrunner.EnsureSandboxConfig(arData); cerr != nil {
								slog.Warn("act_runner sandbox: writing config failed", "err", cerr)
							}
							if dh := resolveActrunnerDockerHost(gctx, fc); dh != "" {
								daemonEnv = append(daemonEnv, "DOCKER_HOST="+dh)
								slog.Info("act_runner sandbox: DOCKER_HOST resolved", "docker_host", dh)
							} else {
								slog.Warn("act_runner sandbox: no DOCKER_HOST — runs-on:sandbox jobs will fail (set actrunner_docker_host or start bashy podman); host-lane jobs still run")
							}
						}
						// Load the operator's vault secrets so CI jobs inherit
						// GITHUB_TOKEN etc. (deploy steps push to GitHub without a
						// per-repo secret — the token stays in the cloudbox vault).
						if vaultEnv := loadVaultSecretsEnv(gctx); len(vaultEnv) > 0 {
							daemonEnv = append(daemonEnv, vaultEnv...)
							slog.Info("act_runner: loaded vault secrets into job env", "count", len(vaultEnv))
						}
						slog.Info("act_runner: starting CI daemon", "instance", instance)
						derr := actrunner.Daemon(gctx, "", arData, daemonEnv...)
						if gctx.Err() != nil {
							return nil
						}
						slog.Warn("act_runner: daemon exited; restarting", "err", derr, "backoff", backoff)
						if !sleepCtx(gctx, backoff) {
							return nil
						}
						backoff = minDur(backoff*2, maxBackoff)
					}
					return nil
				})
			}

			// Mobility-aware continuous directory mirror: each job mirrors a local
			// dir to a peer's mesh service, but ONLY while the pair is reachable
			// (and same-LAN / directly-connected when lan_only) — it pauses when
			// the pair goes remote and resumes + catches up (full sync) when local
			// again. Mobility/dynamic-mesh is the premise. Needs the mesh + the
			// service resolver.
			if fc.MirrorOn() && meshHost != nil && meshResolver != nil {
				jobs := fc.Mirror.Jobs
				sup := &agentmirror.Supervisor{
					Link:   meshLinker{host: meshHost, resolver: meshResolver},
					Logger: slog.Default(),
				}
				g.Go(func() error {
					slog.Info("mirror: supervising jobs (mobility-aware)", "count", len(jobs))
					sup.Run(gctx, jobs)
					return nil
				})
			}

			// Files builtin — embedded File Browser (GUI sibling of /shell +
			// /ssh). In-process handler on a random loopback port, registered
			// as the "files" http app so it rides the existing per-app gate
			// (require_login, lan_only_paths, identity stamping). Read-only +
			// download-only unless files_allow_write is set. Registered here
			// (not in the boot builtin block above) because it needs a
			// listener whose lifetime is tied to the errgroup context.
			if fc.FilesOn() {
				scope := fc.FilesScope
				if scope == "" {
					if home, herr := os.UserHomeDir(); herr == nil {
						scope = home
					}
				}
				// The embedded File Browser is stateless (no DB): scope +
				// write mode come from config, and UI prefs live in the
				// user's browser (localStorage). The only state worth keeping
				// is the session signing key — persisted in agent.json so
				// File Browser sessions survive a daemon restart.
				signingKey, kerr := conf.EnsureFilesSigningKey(cfgPath, fc)
				if kerr != nil {
					slog.Warn("files builtin: signing key", "err", kerr)
				}
				if h, closer, ferr := fbembed.New(fbembed.Options{
					Scope: scope, AllowWrite: fc.FilesAllowWrite, SigningKey: signingKey,
				}); ferr != nil {
					slog.Warn("files builtin: init", "err", ferr)
				} else if ln, lerr := net.Listen("tcp", "127.0.0.1:0"); lerr != nil {
					slog.Warn("files builtin: listen", "err", lerr)
					_ = closer()
				} else {
					port := ln.Addr().(*net.TCPAddr).Port
					filesSrv := &http.Server{Handler: h}
					g.Go(func() error {
						if serr := filesSrv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
							return serr
						}
						return nil
					})
					g.Go(func() error {
						<-gctx.Done()
						sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						_ = filesSrv.Shutdown(sctx)
						return closer()
					})
					if rerr := apps.RegisterFromConfig(conf.AppConfig{
						Name: agent.BuiltinFiles, Scheme: "http", Host: "127.0.0.1", Port: port,
						RequireLogin: true, IndexPath: "/", Enabled: true,
						// No /api/users LAN-fence needed: the embed is stateless
						// (no DB). Perm/scope come from config and every write to
						// the in-memory user no-ops, so a client can't enable
						// write or change scope. UI prefs (view mode, dotfiles,
						// theme…) persist in the user's browser (localStorage),
						// not server-side, so they're naturally per-device.
					}); rerr != nil {
						slog.Warn("files builtin: register", "err", rerr)
					} else {
						slog.Info("files builtin: registered", "scope", scope, "write", fc.FilesAllowWrite, "port", port)
					}
				}
			}
			// LAN inference listener — same-LAN direct inference (P2P
			// serving plane P0). When the operator opted in (lan_inference),
			// bind a LAN-reachable listener that reverse-proxies the OpenAI
			// /v1 + Ollama /api surface to the local inference server
			// (127.0.0.1:11434 — Ollama, or the shard leader's llama-server),
			// so a same-LAN caller reaches this host's LLM directly, bypassing
			// the cloudbox relay for lower latency. This is a LAN-TRUST
			// endpoint: it is NOT authenticated per-request — the operator
			// opting in accepts that the LAN is trusted. Untrusted / org
			// networks leave it off and use the Bearer-authed cloudbox /v1
			// gateway. Non-fatal: a bind failure logs + degrades (the daemon
			// keeps running; only the direct-LAN shortcut is unavailable).
			if fc.LANInferenceOn() {
				target := ollamaURL
				if target == "" {
					target = "http://127.0.0.1:11434"
				}
				lanPort := fc.LANInferencePortOrDefault()
				if tu, perr := url.Parse(target); perr != nil {
					slog.Warn("lan inference: bad target URL — skipping", "target", target, "err", perr)
				} else {
					rp := httputil.NewSingleHostReverseProxy(tu)
					mux := http.NewServeMux()
					mux.Handle("/v1/", rp)
					mux.Handle("/api/", rp)
					addr := fmt.Sprintf("0.0.0.0:%d", lanPort)
					if ln, lerr := net.Listen("tcp", addr); lerr != nil {
						slog.Warn("lan inference: listen failed — continuing without direct-LAN inference", "addr", addr, "err", lerr)
					} else {
						lanSrv := &http.Server{Handler: mux}
						slog.Info("lan inference: serving direct same-LAN inference (LAN-trust, no per-request auth)", "addr", addr, "target", target)
						g.Go(func() error {
							if serr := lanSrv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
								return serr
							}
							return nil
						})
						g.Go(func() error {
							<-gctx.Done()
							sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							return lanSrv.Shutdown(sctx)
						})
					}
				}
			}

			// Folder-watcher scheduler. Runs whether or not Backup
			// is enabled — Apply with a nil/disabled config just
			// keeps the cron with no entries; the admin UI can
			// enable it without a restart.
			g.Go(func() error { return backupSched.Run(gctx) })

			// Peer-plane locality service: start the probe loop. The service
			// itself was created above (before admincore.New) so its measured-
			// tier snapshot feeds SafeView + the outpost_peer_tiers MCP tool.
			if peerPlaneSvc != nil {
				g.Go(func() error {
					if err := peerPlaneSvc.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
						slog.Warn("peerplane: exited", "err", err)
					}
					return nil
				})
			}

			// libp2p mesh host — the peer data plane (constructed above so
			// its status feeds SafeView). Closes on ctx cancel.
			if meshHost != nil {
				g.Go(func() error {
					if err := meshHost.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
						slog.Warn("mesh: exited", "err", err)
					}
					return nil
				})
			}
			if meshRdv != nil {
				g.Go(func() error {
					if err := meshRdv.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
						slog.Warn("mesh rendezvous: exited", "err", err)
					}
					return nil
				})
			}

			// Shard manager — keeps a launch-ready candidate ring up to date
			// (same-LAN owner peers discovered via the peer-plane). Discovery
			// only; forming a shard is gated on a too-big model (v1d).
			// Constructed earlier (so the admincore shard closures capture it);
			// started here under the errgroup.
			if shardMgr != nil {
				g.Go(func() error {
					if err := shardMgr.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
						slog.Warn("shard manager: exited", "err", err)
					}
					return nil
				})
				// LAN peer-to-peer model transfer: serve THIS node's on-disk model
				// store (manifests + blobs) read-only over the mesh, so a peer
				// provisioning a model it lacks fetches the GGUF from here instead
				// of re-pulling from the ollama registry. Mirrors ServeControl.
				if meshHost != nil {
					if cleanup, err := serveModelBlobs(meshHost.Forwarder()); err != nil {
						slog.Warn("model-blobs: serve failed", "err", err)
					} else {
						g.Go(func() error {
							<-gctx.Done()
							cleanup()
							return nil
						})
					}
				}
			}

			// Considerate warm-serving plane: the load profiler (samples +
			// learns the baseline) and the warm supervisor (the yield/restore
			// loop). Both self-disable when their gate wasn't met (nil).
			if loadProfiler != nil {
				g.Go(func() error { return loadProfiler.Run(gctx) })
			}
			if warmExec != nil {
				g.Go(func() error { return warmExec.RunSupervisor(gctx) })
			}

			// DKS image-recipe build agent (P3 of docs/dks-image-recipe-
			// distribution-design.md): polls cloudbox's recipe index and builds
			// each recipe natively with `bashy podman`, loading it into this
			// node's k3s containerd — "recipes, not blobs". Cluster-agent only
			// (needs the <name>-runtime container's containerd) + paired.
			if fc.ClusterOn() && fc.Cluster.HasAgentRuntime() && fc.AccessToken != "" && fc.AgentName != "" {
				g.Go(func() error {
					bashyBin, berr := bashyResolver.Path(gctx)
					if berr != nil {
						slog.Warn("recipebuilder: bashy unavailable; image-recipe builder disabled", "err", berr)
						<-gctx.Done()
						return nil
					}
					return recipebuilder.New(recipebuilder.Config{
						CloudboxBase:     cloudboxHTTPBase(fc),
						AccessToken:      fc.AccessToken,
						RuntimeContainer: fc.ClusterNodeName() + "-runtime",
						BashyBin:         bashyBin,
					}).Run(gctx)
				})
			}

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

			// Load the SSH host key when either the SSH builtin or
			// discovery is on — the key file doubles as the discovery
			// PeerID anchor (fingerprint published in mDNS TXT and the
			// HTTP /hello payload). Generated on first use (ed25519);
			// persists across re-pairings so clients' known_hosts entries
			// stay valid.
			var sshHostKey ssh.Signer
			if fc.SSHOn() || fc.DiscoveryOn() {
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

			// Bootstrap OTEL providers. No-op when
			// OTEL_EXPORTER_OTLP_ENDPOINT is unset (the global W3C
			// propagator still gets installed so the matrix-tunnel
			// envelope contract — traceparent preservation across
			// the proxy hop — works regardless of whether anyone is
			// listening for spans on this hop). Idempotent via
			// sync.Once so an in-process restart doesn't double-up.
			otelProv, otelErr := telemetry.Init(gctx)
			if otelErr != nil {
				slog.Warn("outpost: telemetry init failed; continuing without OTEL", "error", otelErr)
			}
			if otelProv != nil {
				defer func() {
					shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer shCancel()
					_ = otelProv.Shutdown(shCtx)
				}()
			}

			engine := gin.Default()
			// Generic tracing middleware — every proxied app, every
			// builtin route (/shell, /ssh, /apps, /healthz), every
			// inbound cloudbox call gets a span linked to whatever
			// traceparent it arrived with. No-op when telemetry.Init
			// ran in no-op mode.
			engine.Use(telemetry.Tracing("outpost"))
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
				MountWarmRoute: func(rg *gin.RouterGroup) {
					warm.MountRoute(rg, warmExec)
				},
				MountRepairRoute: func(rg *gin.RouterGroup) {
					repair.MountRoute(rg, repairExec)
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
				ClusterInfo: func() any {
					// Re-read cluster config on every poll so a
					// post-pair refresh flows to cloudbox without
					// a daemon restart.
					cur, _ := conf.LoadFile(cfgPath)
					if cur == nil || cur.Cluster == nil {
						return map[string]any{}
					}
					return map[string]any{
						"agent":              cur.Cluster.Runtimes.Agent,
						"virtual":            cur.Cluster.Runtimes.Virtual,
						"kubelet_proxy_port": cur.Cluster.KubeletProxyPort,
						"k8s_api_port":       cur.Cluster.K8sAPIPort,
					}
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
			if fc.ClusterOn() && fc.Cluster.HasAgentRuntime() {
				apiPort := fc.Cluster.K8sAPIPort
				if apiPort == 0 {
					apiPort = 6443
				}
				if fc.Cluster.STCPSecret == "" {
					slog.Warn("cluster agent runtime: STCPSecret empty; visitor disabled (re-pair to fetch from cloudbox)")
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
			// Kubelet routing is published from inside the k3s-agent
			// runtime container (see internal/agent/runtime/image/
			// entrypoint.sh, which adds a [[proxies]] block to
			// /tmp/frpc.toml when OUTPOST_KUBELET_PORT is non-zero).
			// The host-side daemon can't reach the kubelet's 127.0.0.1
			// — different netns — so this publish lives where the
			// kubelet does.

			tunnel, err := agent.NewTunnel(agent.TunnelConfig{
				ServerAddr: cfg.ServerAddr,
				ServerPort: cfg.ServerPort,
				Protocol:   cfg.Protocol,
				Token:      cfg.Token,
				User:       cfg.AgentName,
			}, proxies, visitors, nil)
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

			// Wave 3A: optional LAN-direct SSH listener + LAN peer
			// discovery. Both off by default; see discovery_wiring.go.
			startLANSSHListener(gctx, g, fc, cfg, sshHostKey, peers, apps)
			startLANSSHWSListener(gctx, g, fc, cfg, sshHostKey, peers, apps)
			// Same SSH server, published to authenticated mesh peers over a
			// loopback bind. This is what lets `outpost ssh`/`scp` take the
			// direct peer link instead of the cloudbox relay; without it the
			// mesh rung in `outpost reach` would be a report with nothing
			// behind it. No LAN exposure and no new gate: unreachable until a
			// mesh peer dials it, and still behind peer-ticket / OS password.
			if meshHost != nil {
				fwd := meshHost.Forwarder()
				startMeshSSHWSListener(gctx, g, fc, cfg, sshHostKey, peers, apps,
					func(service, addr string) { fwd.Expose(service, addr) },
					func(service string) bool {
						snap := fwd.Snapshot()
						_, ok := snap.Exposed[service]
						return ok
					})
			}
			startDiscovery(gctx, g, fc, cfg, sshHostKey)

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
					// Capacity source is the Service, not the Counter: Service
					// composes the v2 CapacityReport (counter snapshot + the
					// watcher's own /api/ps cache for loaded_models/swapping)
					// so the registry push and the /_pool/capacity probe
					// emit the same shape. Service.SetWatcher below closes
					// the loop; Snapshot tolerates a nil watcher and just
					// returns the counter snapshot until then.
					ocfg := ollama.Config{
						AgentName:   cfg.AgentName,
						Version:     agent.ReadBuildInfo().Short(),
						OllamaURL:   ollamaURL,
						CloudboxURL: cbBase,
						AccessToken: fc.AccessToken,
						Capacity:    ollamaSvc,
					}
					// Warm-serving advertisement: fold this host's live
					// warm-preload budget (0 when busy) + busy state into each
					// registry push so cloudbox can make considerate warm
					// decisions. Advisory — the /admin/warm executor re-checks
					// the live budget before actually loading.
					if loadProfiler != nil {
						ocfg.WarmStatus = func() (int64, bool) {
							return loadProfiler.WarmBudgetBytes(hostMemTotal), loadProfiler.Busy()
						}
					}
					// Same-LAN direct inference: when the operator opted in,
					// advertise this host's LAN inference URL so cloudbox can
					// hand it to same-LAN callers (they reach the LLM directly,
					// bypassing the relay). Empty when no private LAN IPv4 is
					// found — omitempty drops the field and cloudbox advertises
					// nothing. The listener itself is wired below in the errgroup.
					if fc.LANInferenceOn() {
						ocfg.LANEndpoint = ollama.LANEndpoint(fc.LANInferencePortOrDefault())
					}
					// When an intra-home cluster backend is configured,
					// attach its descriptor source so each push advertises
					// "this home can serve a model up to N bytes" to
					// cloudbox's tier-0 router. Absent ⇒ single-machine
					// push, unchanged.
					if clusterDetector != nil {
						ocfg.Cluster = clusterSourceAdapter{clusterDetector}
					}
					// Advertise an actively-served sharded model into the pool
					// so cloudbox's routing/LB sends requests for it to this
					// (leader) node — sharding fuses into the existing pool.
					if shardMgr != nil {
						ocfg.Cluster = shardClusterSource{base: ocfg.Cluster, mgr: shardMgr}
					}
					w, werr := ollama.New(ocfg)
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
			// Fleet-upgrade pull trigger — the catch-up half of the
			// rollout. The push trigger (cloudbox POSTing /admin/upgrade
			// during a release fan-out) only reaches hosts online at
			// that moment; a host that was asleep or offline reconciles
			// against the latest release on its next poll after the
			// tunnel reconnects. Only spun up when paired (upgradeWorker
			// is built solely when fc.AccessToken != ""); the puller
			// respects update_mode via Worker.Apply, so a "manual" /
			// "never" host polls but never self-upgrades.
			if upgradeWorker != nil {
				if cbBase := cloudboxHTTPBase(fc); cbBase != "" {
					bi := agent.ReadBuildInfo()
					puller := upgrade.PullerConfig{
						CloudboxBase: cbBase,
						AccessToken:  fc.AccessToken,
						Platform:     bi.OS + "_" + bi.Arch,
						Worker:       upgradeWorker,
					}
					g.Go(func() error { return puller.Run(gctx) })
				}
				// Auto-rollback confirm half: if this boot is a just-upgraded
				// binary, ArmConfirm clears the watchdog marker once we've
				// stayed up long enough (declaring the upgrade healthy). A
				// crash before then leaves the marker for the supervisor to
				// act on. No-op when there's no pending upgrade.
				if upgradeConfirmPath != "" {
					g.Go(func() error {
						upgrade.ArmConfirm(gctx, upgradeConfirmPath, agent.ReadBuildInfo().ShortCommit(), upgradeLedger)
						return nil
					})
				}
			}

			// Fleet inventory push — the tool/agent/skill counterpart of the
			// ollama model registry above. The host is the source of truth
			// about what it can run; cloudbox caches that behind a freshness
			// clock so a peer can ask "which host has codex" without probing
			// every machine. Paired-only: the push needs an access token
			// carrying fleet:registry.
			if fc.AccessToken != "" {
				if cbBase := cloudboxHTTPBase(fc); cbBase != "" {
					fw, ferr := fleetreg.New(fleetreg.Config{
						CloudboxURL: cbBase,
						AccessToken: fc.AccessToken,
						AgentName:   fc.AgentName,
						Cluster: func() *fleetreg.ClusterInfo {
							return clusterReport(fc, cbBase)
						},
					})
					if ferr != nil {
						slog.Warn("fleet registry: watcher init failed", "err", ferr)
					} else {
						g.Go(func() error {
							fw.Run(gctx)
							return nil
						})
					}
				}
			}

			// Sandbox image prewarmer — keeps the runnable images pulled so
			// a remote sandbox create+start skips the pull cost. Started
			// here under the errgroup; ctx cancellation stops it. Nil when
			// the sandbox builtin is off or no images are configured.
			if sbPrewarmer != nil {
				g.Go(func() error {
					if err := sbPrewarmer.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
						slog.Warn("sandbox prewarm: exited", "err", err)
					}
					return nil
				})
			}

			// Roadmap #11: cloudbox-as-CA host cert refresh.
			// Independent of cluster mode — every paired outpost
			// benefits from cert-bound peer trust on the discovery
			// /probe surface. Failures (cloudbox unreachable, CA
			// endpoint not yet deployed) are non-fatal: we fall
			// back to TOFU-only trust on peer probes.
			if fc.AccessToken != "" && fc.AgentName != "" && sshHostKey != nil {
				if cbBase := cloudboxHTTPBase(fc); cbBase != "" {
					mgr, mErr := certs.NewManager(certs.Config{
						CloudboxBase: cbBase,
						AccessToken:  fc.AccessToken,
						Principal:    fc.AgentName,
						HostKey:      sshHostKey,
						OnRefresh: func(cert, caPubkey string) error {
							cur, lerr := conf.LoadFile(cfgPath)
							if lerr != nil || cur == nil {
								return lerr
							}
							if cur.Cluster == nil {
								cur.Cluster = &conf.ClusterConfig{}
							}
							cur.Cluster.HostCert = cert
							cur.Cluster.CAPubkey = caPubkey
							return conf.SaveFile(cfgPath, cur)
						},
					})
					if mErr != nil {
						slog.Warn("certs: setup failed (skipping)", "err", mErr)
					} else {
						g.Go(func() error {
							if err := mgr.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
								slog.Warn("certs: manager exited", "err", err)
							}
							return nil
						})
					}
				}
			}

			// Layer-2 selfcheck — validates agent.json + host key +
			// daemon secrets at boot and every 5 min. Auto-regenerates
			// MCP bearer / admin session key when missing; reports
			// (does NOT auto-rotate) the SSH host key when absent.
			// Status feeds the Layer-5 heartbeat payload below.
			scheck := selfcheck.New(cfgPath)
			g.Go(func() error {
				if err := scheck.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("selfcheck: worker exited", "err", err)
				}
				return nil
			})

			// Layer-5 active-push heartbeat. Independent liveness
			// signal to cloudbox — runs even when the matrix tunnel
			// is degraded or restarting. Disabled silently when
			// unpaired (heartbeat.Worker checks at Run entry).
			if fc.AccessToken != "" && fc.AgentName != "" {
				cbBase := cloudboxHTTPBase(fc)
				if cbBase != "" {
					hb := heartbeat.New(heartbeat.Config{
						CloudboxBase:      cbBase,
						AccessToken:       fc.AccessToken,
						AgentName:         fc.AgentName,
						BuildCommit:       agent.ReadBuildInfo().Short(),
						BuildVersion:      agent.ReadBuildInfo().Short(),
						SelfcheckStatusFn: scheck.LastStatus,
					})
					g.Go(func() error {
						if err := hb.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
							slog.Warn("heartbeat: worker exited", "err", err)
						}
						return nil
					})
				}
			}

			// peerimage: serve this node's published recipes to peers over the
			// EXISTING mesh forwarder — a loopback-only listener Expose()d under
			// the configured allowlisted service name. No second overlay, and the
			// forwarder's allowlist boundary is unchanged (the exposure is the
			// deliberate peer_image.service setting). Recipes ride the mesh;
			// image blobs never do.
			if peerImageSvc != nil && meshHost != nil {
				g.Go(func() error {
					ln, lerr := net.Listen("tcp", transferLoopback+":0")
					if lerr != nil {
						slog.Warn("peerimage: recipe index listen failed; peers cannot fetch recipes from this node", "err", lerr)
						<-gctx.Done()
						return nil
					}
					srv := &http.Server{Handler: peerImageSvc.IndexHandler()}
					go func() { _ = srv.Serve(ln) }()
					svcName := fc.PeerImageServiceOrDefault()
					meshHost.Forwarder().Expose(svcName, ln.Addr().String())
					slog.Info("peerimage: recipe index serving", "addr", ln.Addr().String(), "service", svcName)
					<-gctx.Done()
					meshHost.Forwarder().Unexpose(svcName)
					_ = srv.Close()
					return nil
				})
			}

			// Cluster mode: join the cloudbox virtual-podman cluster as a
			// virtual node. Independent of the PodmanEnabled reverse-proxy
			// app — cluster mode dials the local podman socket directly
			// for libpod calls and doesn't need the /app/podman/* route to
			// be mounted. Boot errors are logged but never fatal: a
			// half-configured cluster section shouldn't prevent the rest
			// of outpost from running.
			// Control-plane tunnel server. Gated on ControlPlaneOn alone —
			// NOT on ClusterOn or on any runtime being configured. Hosting
			// the apiserver and joining the cluster are separate decisions,
			// and a control plane that refused to accept workers because the
			// operator had not also given this host a runtime would be a
			// confusing failure: the workers' symptom would be "connection
			// refused" with nothing wrong on their side.
			startControlPlaneTunnel(gctx, g, fc, cfgPath)

			if runtimeErr := fc.Cluster.ValidateRuntimes(); fc.ClusterOn() && runtimeErr != nil {
				slog.Warn("cluster: disabled by invalid runtime configuration", "err", runtimeErr)
			} else if fc.ClusterOn() {
				// The real agent and all selected virtual backends are
				// independent nodes owned by this registered host.
				if fc.Cluster.HasAgentRuntime() {
					if err := startK3sAgentRunner(gctx, g, fc); err != nil {
						slog.Warn("cluster agent runtime: disabled", "err", err)
					}
				}
				if len(fc.Cluster.VirtualRuntimes()) > 0 {
					// Retried rather than one-shot: at boot the OS service
					// starts outpost before any login-time container runtime,
					// so the first attempt routinely finds no podman. A single
					// failed probe used to leave cluster mode off until someone
					// restarted the daemon. Runs in the background so bringing
					// a cold runtime up never holds the tunnel or admin UI.
					g.Go(func() error {
						joinClusterWithRetry(gctx, g, fc, cfgPath, apps, peerPlaneSvc, loadProfiler)
						return nil
					})
				}
				// Materialize the kubectl-ready kubeconfig on disk so
				// the operator gets `kubectl get nodes` and `helm list`
				// without running anything extra. Failures land in
				// userkube.LastStatus (visible in the admin UI's
				// Cluster section); we don't gate vkpodman startup on
				// success — agent-side credentials work independently.
				if path, err := userkube.FetchUserAndWrite(gctx, cloudboxHTTPBase(fc), fc.AccessToken, ""); err != nil {
					slog.Warn("cluster mode: user kubeconfig write failed (admin UI will show the error)", "err", err, "path", path)
				} else {
					slog.Info("cluster mode: user kubeconfig ready", "path", path)
				}
			} else {
				// Cluster disabled (e.g. after `outpost cluster leave`): ensure no
				// stale agent-runtime container lingers with a k3s kubelet
				// retry-looping on a Node cloudbox already deleted. A running
				// kubelet only re-registers at startup, so a deleted Node it can't
				// see means an endless "node not found" loop until the container
				// stops — which nothing does unless we do it here. runtime.Down is
				// a no-op when the container is absent or podman isn't installed,
				// so it's safe on every cluster-off boot.
				if nn := fc.ClusterNodeName(); nn != "" {
					if err := runtime.Down(gctx, runtime.Options{AgentName: nn}); err != nil {
						slog.Warn("cluster disabled: agent runtime teardown", "err", err)
					}
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
// "real shared k8s" agent runtime.
//
// Setup errors return; the long-running loop's errors flow through the
// errgroup the same way the tunnel's do. Like startClusterRunner, we
// never make an agent-runtime boot failure fatal to the outpost.
func startK3sAgentRunner(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig) error {
	cc := fc.Cluster
	if cc == nil {
		return errors.New("cluster agent runtime: no Cluster config")
	}
	if cc.NodeToken == "" {
		return errors.New("cluster agent runtime: NodeToken empty (re-pair to refresh)")
	}
	if cc.STCPSecret == "" {
		return errors.New("cluster agent runtime: STCPSecret empty (re-pair to refresh)")
	}
	apiPort := cc.K8sAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}
	nodeName := fc.ClusterNodeName()
	if nodeName == "" {
		return errors.New("cluster agent runtime: NodeName empty (agent_name unset?)")
	}

	// Refactor (post-Phase 3): the kubelet runtime moved into a
	// privileged podman container so the same model works on Linux
	// (native) AND macOS/Windows (Docker Desktop / Rancher Desktop /
	// ycode-podman / Lima). The outpost daemon stays on the host;
	// only the container hosts k3s-agent + tailscaled + CNI. One
	// outpost = one identity = one Node (named after the outpost).
	// WHICH control plane this node joins. Defaults to the cloudbox pairing;
	// a configured join_endpoint points it at a peer-hosted plane instead.
	//
	// Deriving this from JoinTarget rather than reading fc.ServerAddr
	// directly is what decouples "which cluster do I join" from "which
	// cloudbox am I paired with" — previously the same fields, so joining a
	// peer's cluster meant unpairing from cloudbox and losing apps, shell and
	// the LLM pool along with it.
	tunnelHost, tunnelPort, tunnelToken, serverUser := fc.JoinTarget()
	frpProtocol := ""
	if cc.JoinsPeerPlane() {
		// A peer's frps is plain TCP: there is no TLS-terminating edge in
		// front of it, and wss would fail the handshake rather than degrade.
		frpProtocol = "tcp"
	}

	rtOpts := runtime.Options{
		AgentName:          nodeName,
		HostName:           fc.AgentName,
		NodeToken:          cc.NodeToken,
		APIServer:          fmt.Sprintf("https://127.0.0.1:%d", apiPort),
		APIPort:            apiPort,
		KubeletPort:        cc.KubeletProxyPort,
		CloudboxHost:       tunnelHost,
		CloudboxPort:       tunnelPort,
		STCPSecret:         cc.STCPSecret,
		MatrixToken:        tunnelToken,
		FRPProtocol:        frpProtocol,
		FRPServerUser:      serverUser,
		PodCIDR:            cc.OverlayPodCIDR,
		OverlayLoginServer: cc.OverlayLoginServer,
		OverlayAuthKey:     cc.OverlayAuthKey,
		PeerFlannel:        cc.JoinsPeerPlane(),
	}
	if cc.JoinsPeerPlane() && len(cc.VirtualRuntimes()) > 0 {
		// The authenticated peer visitor lives inside the physical agent's
		// runtime container. Publish that exact listener back to host loopback
		// for the host-side vk runners; host 6443 may already belong to the
		// legacy cloud visitor, and hosted planes reserve 16443.
		rtOpts.APIBridgeHostPort = runtime.DefaultPeerAPIBridgePort
	}

	// A peer-joined worker needs a tailnet identity, and JoinPeerPlane
	// deliberately CLEARS the cloudbox overlay trio at join time (they
	// describe the cloud plane and would attach this node to the wrong
	// overlay). So the credential has to be re-acquired here, at boot,
	// from the party that can still mint one: cloudbox, over the access
	// token, which is unaffected by anything that broke the tailnet.
	//
	// Fetched rather than configured on purpose — an auth key is a
	// standing credential to join the tailnet, which is what reaches the
	// pod network. Putting it on a CLI flag would put it in shell
	// history, process listings and support pastes; every fetch here is a
	// fresh single-use key that lives only in this process and the
	// container's env.
	//
	// Every precondition fails CLOSED, before runtime.Up: an unpaired
	// host has nothing to ask cloudbox with, and a failed fetch leaves
	// this node without the tailnet identity flannel-over-tailscale
	// needs. Both return a non-nil error rather than nil, so this
	// function's caller — which already logs "cluster agent runtime:
	// disabled" with that error attached — is what reports it; there is
	// nothing left here to claim about automatic retry.
	if err := requirePeerOverlayAccessToken(cc, fc.AccessToken); err != nil {
		return err
	}
	if cc.JoinsPeerPlane() {
		// Relay preconditions FIRST, before the auth-key fetch: a fetch
		// mints a single-use key, and failing on a missing relay afterwards
		// would burn one per boot for nothing.
		if err := applyPeerOverlayRelay(fc, &rtOpts); err != nil {
			return err
		}
		cl := &overlaykey.Client{
			BaseURL:     cloudboxHTTPBase(fc),
			AccessToken: fc.AccessToken,
			AgentName:   nodeName,
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := preparePeerOverlay(fetchCtx, cl, &rtOpts, nodeName)
		cancel()
		if err != nil {
			// Do NOT fall through to runtime.Up. Starting peer-flannel
			// without a tailnet identity gives --flannel-iface=tailscale0
			// no address: the node registers, goes Ready, and drops every
			// pod packet — the dark node this slice exists to prevent.
			// Logging the failure and continuing would produce exactly that.
			return err
		}
	}

	// Announce whether this node gets a real (per-node, routable) pod
	// network or the shared-range single-node fallback. Both come up
	// Ready and look identical from the cluster's side, so the WARN on
	// the fallback path is the only signal an operator gets before pod
	// IPs start colliding across nodes.
	rtOpts.PodNetwork().Log(nodeName)

	// Self-rebuild the runtime image when this binary differs from the one
	// that produced the current image (most importantly right after an
	// `outpost upgrade`), so CNI / entrypoint changes reach the node with
	// no manual `outpost cluster build-runtime`. Cheap in the steady state
	// (one image inspect); non-fatal — if the rebuild fails, Up proceeds
	// with whatever image is present and reports clearly if there is none.
	if cacheDir, derr := os.UserCacheDir(); derr == nil {
		stateDir := filepath.Join(cacheDir, "outpost")
		if rebuilt, eerr := runtime.EnsureImage(ctx, runtime.BuildOptions{},
			agent.ReadBuildInfo().ShortCommit(), stateDir); eerr != nil {
			slog.Warn("cluster agent runtime: image ensure failed; using existing image if present", "err", eerr)
		} else if rebuilt {
			rtOpts.ForceRecreate = true
			slog.Info("cluster agent runtime: image rebuilt to match this outpost version")
		}
	}

	if err := runtime.Up(ctx, rtOpts); err != nil {
		if errors.Is(err, runtime.ErrPodmanNotFound) {
			slog.Warn("cluster agent runtime: " + err.Error())
			return nil
		}
		slog.Warn("cluster agent runtime: start failed", "err", err)
		return nil
	}
	g.Go(func() error {
		// Tail logs AND keep the container alive: runtime.TailLogs
		// returns when the container exits (or the stream breaks), and
		// the loop re-Ups it with backoff. See superviseAgentRuntime.
		superviseAgentRuntime(ctx, rtOpts)
		return nil
	})

	// Keep this node registered on the overlay without human help.
	//
	// Overlay credentials used to arrive only at pairing and reattach,
	// and reattach runs at daemon BOOT. So anything that invalidated the
	// tailnet registration afterwards — a cloudbox deploy resetting
	// Headscale, most commonly — left the node off the pod network until
	// someone restarted the daemon. The refresher notices and asks for a
	// fresh single-use key over the cloudbox access token, which is
	// unaffected by whatever broke the overlay.
	//
	// Gated on having overlay credentials AND a cloudbox token: a host
	// that never joined an overlay has nothing to refresh.
	if overlayRefresherWanted(rtOpts, fc.AccessToken) {
		refresher := &overlaykey.Refresher{
			Client: &overlaykey.Client{
				BaseURL:     cloudboxHTTPBase(fc),
				AccessToken: fc.AccessToken,
				AgentName:   nodeName,
			},
			PodCIDR: rtOpts.PodCIDR,
			// Peer-flannel workers must never advertise cloudbox's
			// cluster-carved PodCIDR fetched by a Heal call — see
			// overlaykey.Refresher.IgnoreCredentialPodCIDR.
			IgnoreCredentialPodCIDR: rtOpts.PeerFlannel,
			// Heal runs `tailscale up` inside the runtime container, where
			// the entrypoint's socat TLS terminator fronts the frp
			// overlay-control visitor — the Cloudflare-free path to
			// Headscale (ts2021 needs TLS even on loopback). Must match the
			// entrypoint's OVERLAY_CONTROL_PORT default (8091).
			ControlURL: "https://127.0.0.1:8091",
			Exec: func(ectx context.Context, args ...string) ([]byte, error) {
				return runtime.ExecInContainer(ectx, rtOpts, args...)
			},
		}
		g.Go(func() error { return refresher.Run(ctx) })
	}
	return nil
}

// overlayRefresherWanted reports whether this node should run the overlay
// key refresher: it needs an effective login server AND a cloudbox access
// token to fetch fresh keys with.
//
// Gating on rtOpts.OverlayLoginServer — the EFFECTIVE value the runtime
// container was actually started with — rather than cc.OverlayLoginServer
// is load-bearing for peer-hosted-plane workers. JoinPeerPlane clears
// cc.OverlayLoginServer at join time (it described the cloud plane, and
// would attach this node to the wrong overlay), but preparePeerOverlay
// re-populates rtOpts.OverlayLoginServer from a freshly fetched peer-plane
// credential before this check runs. Gating on the cleared config field
// would leave every peer-flannel worker's overlay registration to rot
// after the first cloudbox-side reset, silently, since nothing here would
// have ever run. The cloudbox-hosted plane is unaffected either way:
// rtOpts.OverlayLoginServer is seeded straight from cc.OverlayLoginServer
// and nothing downstream mutates it.
func overlayRefresherWanted(rtOpts runtime.Options, accessToken string) bool {
	return strings.TrimSpace(rtOpts.OverlayLoginServer) != "" && strings.TrimSpace(accessToken) != ""
}

// requirePeerOverlayAccessToken fails closed when a peer-hosted plane join
// cannot fetch tailnet credentials: cloudbox is the only party that can mint
// one, and an unpaired host (no access token) has nothing to ask with. The
// cloudbox-hosted plane never hits this precondition — it already carries
// its own overlay trio through pairing, and re-fetching there would burn a
// key it does not need.
//
// This is a precondition, not an optional skip: a peer-plane join with no
// token used to fall straight through to runtime.Up with PeerFlannel set and
// no tailnet identity to give it — the same dark-node failure a fetch error
// produces, just reached by a different door. Returning an error here closes
// that door before runtime.Up ever runs.
func requirePeerOverlayAccessToken(cc *conf.ClusterConfig, accessToken string) error {
	if cc == nil || !cc.JoinsPeerPlane() {
		return nil
	}
	if strings.TrimSpace(accessToken) == "" {
		return errors.New("cluster agent runtime: peer-hosted plane needs a cloudbox access token to " +
			"fetch a tailnet identity for flannel-over-tailscale; none present (unpaired host?)")
	}
	return nil
}

// applyPeerOverlayRelay threads the overlay-relay session into opts: the
// second in-container frpc session, dialed DIRECTLY at cloudbox, that
// carries the overlay-control STCP visitor a peer-joined worker's ts2021
// tailnet registration rides (docs/adr-peer-dks-pod-network.md — the
// registration hop the ADR left unresolved). The endpoint is the cloudbox
// PAIRING (ServerAddr/Port/Protocol/Token — unaffected by the peer join);
// the visitor secret is cloudbox's cluster STCP secret retained in
// cluster.cloud_stcp_secret, because the peer join overwrote STCPSecret
// with the peer's credential.
//
// Fails CLOSED on a missing secret, same doctrine as the auth-key fetch: a
// peer-flannel container without a path to Headscale cannot get a tailnet
// IPv4, so its entrypoint gate would exit anyway — this just says WHY
// before a container ever starts, and without consuming a single-use key.
func applyPeerOverlayRelay(fc *conf.FileConfig, opts *runtime.Options) error {
	secret := strings.TrimSpace(fc.Cluster.CloudSTCPSecret)
	if secret == "" {
		return errors.New("cluster agent runtime: peer-hosted plane has no cloudbox overlay-relay secret " +
			"(cluster.cloud_stcp_secret) — without it the tailnet registration cannot reach cloudbox's " +
			"Headscale and flannel-over-tailscale has no underlay. The secret is captured automatically " +
			"from cloudbox at pairing, boot reattach, or peer join; ensure this host is paired and cloudbox " +
			"reachable, then restart the daemon")
	}
	if strings.TrimSpace(fc.ServerAddr) == "" {
		return errors.New("cluster agent runtime: peer-hosted plane needs the cloudbox pairing " +
			"(server_addr) to relay the tailnet registration; none present")
	}
	opts.OverlayRelayHost = fc.ServerAddr
	opts.OverlayRelayPort = fc.ServerPort
	opts.OverlayRelayProtocol = fc.Protocol
	opts.OverlayRelayToken = fc.Token
	opts.OverlayRelaySecret = secret
	opts.OverlayRelayUser = conf.CloudboxPublisherUser
	return nil
}

// superviseAgentRuntime keeps the agent-runtime container alive for the
// daemon's life. runtime.Up is idempotent — a running container is reused
// and a stopped one is started — so the loop is: tail logs until the
// container exits (or the log stream breaks), then re-Up with backoff.
//
// Before this loop, a container exit was terminal until a human restarted
// the daemon. That mattered most for peer-flannel workers: the entrypoint's
// tailscale-IPv4 gate deliberately exits non-zero rather than starting a
// dark node, so a cloudbox outage spanning a container boot killed the
// node until someone noticed. The control-plane container has had the same
// treatment (SuperviseServer) since A1/A3; this is the worker-side half.
func superviseAgentRuntime(ctx context.Context, rtOpts runtime.Options) {
	const initialBackoff = 15 * time.Second
	const maxBackoff = 5 * time.Minute
	backoff := initialBackoff
	// A rebuilt image already forced one recreate in startK3sAgentRunner;
	// respawns must reuse, not recreate on every lap.
	rtOpts.ForceRecreate = false
	for {
		_ = runtime.TailLogs(ctx, rtOpts)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("cluster agent runtime: container log stream ended; re-ensuring container",
			"node", rtOpts.AgentName, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := runtime.Up(ctx, rtOpts); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("cluster agent runtime: restart failed; will retry", "err", err)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = initialBackoff
	}
}

// applyPeerOverlayCredentials fetches a fresh single-use overlay key and
// writes it into opts. It reads ONLY LoginServer and AuthKey.
//
// Credentials.PodCIDR is IGNORED, deliberately. Under the peer-flannel
// decision (docs/adr-peer-dks-pod-network.md, Option A) the pod CIDR comes
// from flannel reading Node.spec.podCIDR, which the PEER's k3s
// controller-manager allocates. Cloudbox's carve describes the
// cloudbox-hosted cluster and has no authority over the peer's; honoring
// it here would hand the entrypoint a CIDR to template a conflist from and
// reintroduce exactly the allocator this ADR removed.
//
// opts is left untouched on error so a failed fetch cannot half-apply.
func applyPeerOverlayCredentials(ctx context.Context, cl *overlaykey.Client, opts *runtime.Options) error {
	creds, err := cl.Fetch(ctx)
	if err != nil {
		return err
	}
	opts.OverlayLoginServer = strings.TrimSpace(creds.LoginServer)
	opts.OverlayAuthKey = strings.TrimSpace(creds.AuthKey)
	opts.PodCIDR = ""
	return nil
}

// preparePeerOverlay fetches the peer-plane overlay credentials into opts,
// logs the outcome SAFELY (presence, never the key — see logPeerOverlayFetch),
// and returns an error when the caller must NOT proceed to start the runtime
// container. It is the whole "may this node come up" decision in one
// pure-ish function so a regression is a failing test rather than a dark node.
//
// A fetch failure always returns a non-nil error, without exception.
// peer-flannel binds --flannel-iface=tailscale0; with no tailnet identity
// that interface has no address, so the node joins, reports Ready, and
// silently blackholes pod traffic. Starting it is strictly worse than not
// starting it: a node that is absent is diagnosable, a node that is present
// and broken is not.
//
// The returned error wraps the underlying overlaykey error with %w, so
// errors.Is(err, overlaykey.ErrOverlayDisabled) / ErrThrottled still resolve
// at the call site.
//
// ErrOverlayDisabled is the case that could argue for continuing — cloudbox
// is not running an overlay at all, so there is no key to be had and no
// outage to wait out. We still refuse, deliberately: "no key exists" and "the
// key fetch failed" put this node in the same place, holding a peer-flannel
// config it cannot satisfy. The difference is operator action, not node
// behavior, and logPeerOverlayFetch already says which one happened and what
// to do about it (enable the overlay, or host this worker on the
// cloudbox-managed plane). Nothing is persisted here, so flipping the overlay
// on cloudbox and restarting is all it takes to come up.
func preparePeerOverlay(ctx context.Context, cl *overlaykey.Client, opts *runtime.Options, node string) error {
	err := applyPeerOverlayCredentials(ctx, cl, opts)
	logPeerOverlayFetch(node, opts.OverlayAuthKey != "", err)
	if err != nil {
		return fmt.Errorf("cluster agent runtime: overlay credential fetch failed for peer-hosted plane: %w", err)
	}
	return nil
}

// logPeerOverlayFetch reports the outcome. It logs PRESENCE, never the
// key: an auth key that reaches a log file is a standing tailnet
// credential sitting in whatever ships those logs.
//
// A failure is ERROR, not WARN, and says what happens next, because the
// container will refuse to start a dark node once it finds tailscale0
// without an address — this line is the explanation for that exit.
func logPeerOverlayFetch(node string, haveKey bool, err error) {
	switch {
	case err == nil:
		slog.Info("cluster: fetched overlay credentials for peer-hosted plane",
			"node", node, "has_auth_key", haveKey)
	case errors.Is(err, overlaykey.ErrOverlayDisabled):
		slog.Error("cluster: cloudbox overlay is DISABLED, so this peer-joined node cannot get a "+
			"tailnet identity. flannel-over-tailscale needs one: the runtime container will refuse "+
			"to start rather than join as a node that drops every pod packet. Enable the overlay on "+
			"cloudbox, or host this worker on the cloudbox-managed plane instead.",
			"node", node)
	case errors.Is(err, overlaykey.ErrThrottled):
		slog.Error("cluster: cloudbox throttled the overlay key request; this peer-joined node has no "+
			"tailnet identity for this boot. The runtime container will refuse to start; retry shortly "+
			"(`outpost restart`) rather than treating it as a network fault.",
			"node", node)
	default:
		// err is an overlaykey error; the package never puts a key in one
		// (see Fetch — it reports an empty key as a status, not a value).
		slog.Error("cluster: overlay credential fetch failed for peer-hosted plane; the runtime "+
			"container will refuse to start rather than join without a tailnet identity",
			"node", node, "err", err)
	}
}

// sandboxPrewarmImages resolves the image set the prewarmer keeps pulled.
// An explicit SandboxPrewarmImages wins; otherwise it falls back to the
// concrete (non-wildcard) entries of the SandboxAllowedImages allowlist —
// pre-pulling exactly the images a caller is permitted to run. A wildcard
// allowlist entry ("repo/*") names no concrete image, so it's skipped.
func sandboxPrewarmImages(fc *conf.FileConfig) []string {
	if fc == nil {
		return nil
	}
	if len(fc.SandboxPrewarmImages) > 0 {
		return fc.SandboxPrewarmImages
	}
	var out []string
	for _, img := range fc.SandboxAllowedImages {
		if strings.Contains(img, "*") {
			continue
		}
		out = append(out, img)
	}
	return out
}

// startClusterRunner validates fc.Cluster, detects the local podman
// socket, and launches vknode.Run on g. Setup errors return; the
// long-running loop's errors flow through the errgroup the same way
// the tunnel's do.
//
// We never make a virtual-runtime boot failure fatal to the agent: a
// half-configured Cluster section shouldn't stop the matrix tunnel or
// admin UI from coming up. The caller logs the returned error and
// moves on.
// joinClusterWithRetry keeps attempting the cluster join until it succeeds or
// the daemon shuts down, with a capped exponential backoff.
//
// The join can fail for reasons that resolve on their own — the container
// runtime not being up yet at boot, a cold podman machine still starting, a
// transient cloudbox blip while fetching credentials. Treating the first
// failure as terminal (the previous behavior) meant cluster mode stayed off
// until a manual restart, which is what made correct startup ordering a
// prerequisite. Retrying removes the ordering requirement entirely.
//
// Returns once the join succeeds; startClusterRunner owns the long-lived
// goroutines from that point on.
func joinClusterWithRetry(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig, cfgPath string, apps *agent.AppRegistry, peerSvc *peerplane.Service, loadProfiler *sysload.Profiler) {
	const (
		firstDelay = 5 * time.Second
		maxDelay   = 2 * time.Minute
	)
	for attempt, delay := 1, firstDelay; ; attempt++ {
		err := startClusterRunner(ctx, g, fc, cfgPath, apps, peerSvc, loadProfiler)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("cluster virtual runtimes: not ready, will retry",
			"err", err, "attempt", attempt, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay = delay * 2; delay > maxDelay {
			delay = maxDelay
		}
	}
}

func startClusterRunner(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig, cfgPath string, apps *agent.AppRegistry, peerSvc *peerplane.Service, loadProfiler *sysload.Profiler) error {
	nodeBase := fc.ClusterNodeName()
	if nodeBase == "" {
		return errors.New("ClusterNodeName empty (agent_name unset?)")
	}
	hostName := strings.TrimSpace(fc.AgentName)
	if hostName == "" {
		return errors.New("AgentName empty")
	}
	modes := fc.Cluster.VirtualRuntimes()
	if len(modes) == 0 {
		return errors.New("cluster: no virtual runtimes configured")
	}

	var podmanSock string
	for _, mode := range modes {
		if mode == conf.ClusterRuntimeVKPodman {
			sock, err := ensurePodmanRuntime(ctx)
			if err != nil {
				return err
			}
			podmanSock = sock
			break
		}
	}

	cc := fc.Cluster
	if cc == nil {
		return errors.New("cluster virtual runtimes: no Cluster config")
	}
	cloudboxBase := cloudboxHTTPBase(fc)
	peerJoin := cc.JoinsPeerPlane()

	var (
		kubeCfg       *rest.Config
		access        *vknode.Access
		tokenFile     string // cloudbox path only
		peerRefresher *vknode.PeerCredentialRefresher
	)

	if peerJoin {
		// PEER-HOSTED PLANE. There is no cloudbox authority here: no
		// FetchKubeconfig to mint a token, no FetchAccess to hand down a
		// namespace allow-set, no cloudbox CA to trust. Everything is derived
		// locally — the PEER's CA identity, a credential minted or held on this
		// host, the apiserver reached at the loopback visitor address the k3s
		// agent runtime already uses, and a fail-closed peer-local namespace
		// policy. None of the cloudbox-only refreshers run on this path.
		cred := vknode.PeerCredential{
			APIURL:     peerVirtualAPIURL(cc),
			CA:         cc.CA,
			Token:      cc.Token,
			ClientCert: cc.ClientCert,
			ClientKey:  cc.ClientKey,
		}
		if err := cred.Validate(); err != nil {
			return fmt.Errorf("cluster virtual runtimes (peer plane): %w", err)
		}
		credDir, err := vknode.DefaultPeerCredentialDir()
		if err != nil {
			return fmt.Errorf("cluster mode: peer credential dir: %w", err)
		}
		files, err := cred.Materialize(credDir)
		if err != nil {
			return fmt.Errorf("cluster mode: materialize peer credential: %w", err)
		}
		kubeCfg, err = cred.RestConfig(files)
		if err != nil {
			return err
		}

		// Fail-closed peer-local namespace policy. Always non-nil: an empty
		// cluster.allowed_namespaces denies every pod rather than falling
		// through to the nil "allow all" gate a cloudbox-less node used to get.
		access = vknode.PeerNamespacePolicy(cc.AllowedNamespaces)
		slog.Info("cluster virtual runtimes: peer namespace policy (fail-closed)",
			"apiserver", cred.APIURL, "credential", cred.CredentialKind(),
			"namespaces", access.Snapshot(), "has_peer_ca", len(cc.CA) > 0)
		if len(cc.AllowedNamespaces) == 0 {
			slog.Warn("cluster virtual runtimes: peer namespace policy is empty — every pod will be denied; set cluster.allowed_namespaces")
		}

		// Local (cloudbox-free) bearer-token rotation. The refresher re-reads
		// the persisted config and rewrites the token file when an operator
		// rotates cluster.token, so client-go picks it up without a restart.
		if cred.Token != "" {
			peerRefresher = vknode.NewPeerCredentialRefresher(
				func() (vknode.PeerCredential, error) {
					reloaded, lerr := conf.LoadFile(cfgPath)
					if lerr != nil {
						return vknode.PeerCredential{}, lerr
					}
					rc := reloaded.Cluster
					if rc == nil {
						return vknode.PeerCredential{}, errors.New("cluster config vanished")
					}
					return vknode.PeerCredential{
						APIURL:     rc.LocalAPIURL(),
						CA:         rc.CA,
						Token:      rc.Token,
						ClientCert: rc.ClientCert,
						ClientKey:  rc.ClientKey,
					}, nil
				}, files, cred)
		}
	} else {
		// CLOUDBOX-HOSTED PLANE (the historical default).
		//
		// Bootstrap: if we have an outpost access_token and either no
		// persisted credentials yet or already-expired ones, ask cloudbox to
		// mint a fresh kubeconfig and persist it. This is the "operator flipped
		// the toggle, never pasted anything" path.
		//
		// A fetch failure here is non-fatal when we already have cached
		// credentials (even expired ones — the refresher will retry on the
		// loop), and fatal otherwise (there's nothing to dial the apiserver
		// with).
		if shouldFetchKubeconfig(fc) && fc.AccessToken != "" && cloudboxBase != "" {
			slog.Info("cluster: fetching shared virtual-node kubeconfig", "host", hostName, "cloudbox", cloudboxBase)
			fetched, err := vknode.FetchKubeconfig(ctx, cloudboxBase, fc.AccessToken, hostName)
			if err != nil {
				if cc.Token == "" {
					return fmt.Errorf("cluster virtual runtimes: fetch kubeconfig: %w", err)
				}
				slog.Warn("cluster virtual runtimes: fetch failed; falling back to cached credentials",
					"err", err, "host", hostName)
			} else {
				persistClusterCredential(fc, cfgPath, fetched)
			}
		}

		if cc.APIURL == "" || cc.Token == "" {
			return errors.New("cluster virtual runtimes: no usable credentials")
		}

		// Write the live bearer token to a file so client-go's transport
		// re-reads it across rotations. Path stays under the agent's
		// cache dir (mode 0600 — same locality as the pidfile + logs).
		var err error
		tokenFile, err = vknode.DefaultTokenFilePath()
		if err != nil {
			return fmt.Errorf("cluster mode: token-file path: %w", err)
		}
		if err := vknode.WriteTokenFile(tokenFile, cc.Token); err != nil {
			return fmt.Errorf("cluster mode: write token-file: %w", err)
		}

		kubeCfg, err = vknode.ConfigFromCluster(cc.APIURL, tokenFile, cc.CA)
		if err != nil {
			return err
		}

		// Namespace-access gate. Cloudbox has already authenticated the
		// opaque host token and owns the namespace mapping, so fetch the
		// authoritative owner + sharee allow-set instead of decoding claims
		// from the token locally. A paired virtual node must not start without
		// this gate.
		if fc.AccessToken != "" {
			resp, err := vknode.FetchAccess(ctx, cloudboxBase, fc.AccessToken, hostName)
			if err != nil {
				return fmt.Errorf("cluster virtual runtimes: fetch namespace access: %w", err)
			}
			access = vknode.NewAccess(resp.AllowedNamespaces...)
			slog.Info("cluster virtual runtimes: namespace access gate",
				"owner_namespace", resp.OwnerNamespace,
				"namespaces", resp.AllowedNamespaces)
		}
	}

	// Locality tier label. Stamp the Node with this host's MEASURED
	// locality (peerplane ground truth) rather than a stub: SelfTier
	// reduces the latest probe snapshot to the host's best-link tier.
	// Only set the label when a probe cycle has actually recorded a peer
	// — an empty snapshot (single machine, or peerplane off) leaves the
	// tier label off entirely, same as before this wiring.
	extraNodeLabels := map[string]string{
		vknode.NodeHostLabel:      hostName,
		"outpost.dhnt.io/runtime": "virtual",
	}
	if peerSvc != nil {
		if snap := peerSvc.Snapshot(); len(snap) > 0 {
			tier := vknode.LocalityTierForMeasured(string(peerplane.BestTier(snap)))
			for key, value := range vknode.NodeLocalityLabels("", tier) {
				extraNodeLabels[key] = value
			}
			slog.Info("cluster: measured locality tier", "host", hostName, "tier", tier, "peers", len(snap))
		}
	}

	cacheBase, err := conf.DefaultCacheDir()
	if err != nil {
		return fmt.Errorf("cluster: virtual runtime data dir: %w", err)
	}
	type virtualRuntimeSpec struct {
		mode    string
		node    string
		backend vknode.Backend
		labels  map[string]string
	}
	specs := make([]virtualRuntimeSpec, 0, len(modes))
	for _, mode := range modes {
		var backend vknode.Backend
		switch mode {
		case conf.ClusterRuntimeVKPodman:
		case conf.ClusterRuntimeVKNative:
			backend, err = vknode.NewNativeProcessBackend(vknode.NativeProcessConfig{
				DataDir: filepath.Join(cacheBase, mode),
			})
		case conf.ClusterRuntimeVKOllama:
			backend, err = vknode.NewOllamaBackend(vknode.OllamaConfig{
				DataDir: filepath.Join(cacheBase, mode),
			})
		default:
			err = fmt.Errorf("unsupported virtual runtime %q", mode)
		}
		if err != nil {
			return fmt.Errorf("cluster runtime %s: backend: %w", mode, err)
		}
		labels := maps.Clone(extraNodeLabels)
		labels["outpost.dhnt.io/backend"] = mode
		specs = append(specs, virtualRuntimeSpec{
			mode:    mode,
			node:    nodeBase + "-" + mode,
			backend: backend,
			labels:  labels,
		})
	}

	// Start only after every backend has initialized successfully. Each runner
	// owns its retry loop: setup succeeding merely means the goroutines were
	// launched, not that a runner can never lose its apiserver connection or
	// exit later.
	for _, spec := range specs {
		spec := spec
		g.Go(func() error {
			runVirtualNodeWithRetry(ctx, spec.node, spec.mode, 5*time.Second, 2*time.Minute,
				func(runCtx context.Context) error {
					slog.Info("cluster: virtual node joining", "node", spec.node, "host", hostName,
						"backend", spec.mode, "apiserver", kubeCfg.Host, "podman_socket", podmanSock)
					var hostLoad func() vknode.HostLoad
					if loadProfiler != nil {
						hostLoad = func() vknode.HostLoad {
							sample := loadProfiler.Current()
							return vknode.HostLoad{
								At: sample.At, CPUPercent: sample.CPUPercent, MemAvailFrac: sample.MemAvailFrac,
							}
						}
					}
					return vknode.Run(runCtx, vknode.RunOptions{
						NodeName:        spec.node,
						PodmanSocket:    podmanSock,
						Backend:         spec.backend,
						Kube:            kubeCfg,
						Access:          access,
						TransientApps:   appsAsTransient{apps},
						ExtraNodeLabels: spec.labels,
						HostLoad:        hostLoad,
					})
				})
			return nil
		})
	}

	// Peer-plane bearer-token rotation (cloudbox-free). Re-reads the persisted
	// config and rewrites the token file when the operator rotates it.
	if peerRefresher != nil {
		g.Go(func() error { return peerRefresher.Run(ctx) })
	}

	// Token rotation. Only spin the cloudbox refresher when we have a working
	// fetch path (access_token + cloudbox URL) AND we are not on a peer plane
	// (whose credentials cloudbox never issued); when the operator pasted a
	// kubeconfig for a non-cloudbox cluster, leave the token alone — they're
	// responsible for replacing it before expiry.
	if !peerJoin && fc.AccessToken != "" && cloudboxBase != "" {
		refresher := vknode.NewRefresher(vknode.RefreshDeps{
			CloudboxBase:  cloudboxBase,
			AccessToken:   fc.AccessToken,
			NodeName:      hostName,
			TokenFilePath: tokenFile,
			OnRotation: func(p *vknode.ParsedKubeconfig) {
				persistClusterCredential(fc, cfgPath, p)
			},
		})
		g.Go(func() error {
			return refresher.Run(ctx, cc.Token)
		})
	}

	// Namespace allow-set refresh. Cloudbox is the source of truth for
	// which sharee namespaces may schedule on this node (owner +
	// HostShare rows with app="podman"). The synchronous fetch above
	// populated the gate before any runner started; this loop keeps it
	// current. Refresh failures preserve the last known allow-set.
	if !peerJoin && access != nil && fc.AccessToken != "" && cloudboxBase != "" {
		accessRefresher := vknode.NewAccessRefresher(vknode.AccessRefreshDeps{
			CloudboxBase: cloudboxBase,
			AccessToken:  fc.AccessToken,
			NodeName:     hostName,
			Access:       access,
		})
		g.Go(func() error { return accessRefresher.Run(ctx) })
	}

	// NOTE: the control-plane reconcilers are NOT started here. This
	// kubeconfig is for the cluster this host JOINS; they must act on the
	// cluster it HOSTS. They are started from startControlPlaneTunnel.

	return nil
}

func runVirtualNodeWithRetry(ctx context.Context, node, backend string, initialBackoff, maxBackoff time.Duration, run func(context.Context) error) {
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		err := run(ctx)
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		if err == nil {
			err = errors.New("runner exited unexpectedly")
		}
		slog.Warn("cluster: virtual runner exited; will retry",
			"node", node, "backend", backend, "err", err,
			"attempt", attempt, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// startControlPlaneTunnel runs the frp SERVER on a host that is the control
// plane, so worker outposts can dial it exactly the way they dial cloudbox.
//
// This is the last piece that made placement a real choice rather than a
// design intent. The worker side was already identical in all three
// placements — frpc dials an frps and binds 127.0.0.1:<api-port> for
// `k3s agent` — and the only thing that did not exist anywhere but cloudbox
// was the server end of that dial.
//
// A no-op on every host that is not the control plane, which is nearly all of
// them.
func startControlPlaneTunnel(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig, cfgPath string) {
	if fc == nil {
		slog.Info("control plane: not hosting (no configuration loaded)")
		return
	}
	// A2 — this decline used to be SILENT, which is why a host that was meant
	// to host the plane produced no log line at all: cluster.control_plane was
	// true in the operator's config, yet the flag failed to reach here because
	// mergePairing keeps only a subset of Cluster fields on register/recover
	// and drops Cluster.ControlPlane. With no log, the operator had nothing to
	// go on while workers failed to join. So say WHY we are not hosting — and
	// escalate to Warn when this host still carries control-plane-only
	// credentials (a minted tunnel token, or a recorded control-plane
	// kubeconfig), since creds-present-but-flag-off is the exact signature of a
	// dropped flag rather than a host that was never a control plane.
	if !fc.Cluster.ControlPlaneOn() {
		if c := fc.Cluster; c != nil &&
			(strings.TrimSpace(c.TunnelToken) != "" ||
				strings.TrimSpace(c.ControlPlaneKubeconfig) != "") {
			slog.Warn("control plane: not hosting — control_plane flag is off, " +
				"but this host carries control-plane credentials; the flag may " +
				"have been dropped on register/recover (re-set cluster.control_plane)")
			return
		}
		slog.Info("control plane: not hosting (cluster.control_plane is off)")
		return
	}
	// Both credentials, minted together: a worker needs the pair, and a plane
	// with only one of them accepts logins and refuses every join.
	token, err := conf.EnsureClusterTunnelToken(cfgPath, fc)
	if err != nil {
		// Non-fatal, like every other cluster boot failure here: a host that
		// cannot mint a token should still serve its apps and shell.
		slog.Warn("control plane: disabled (no tunnel token)", "err", err)
		return
	}
	secret, err := conf.EnsureControlPlaneSTCPSecret(cfgPath, fc)
	if err != nil {
		slog.Warn("control plane: disabled (no stcp secret)", "err", err)
		return
	}
	addr, port := fc.Cluster.TunnelBind()

	// THE CONTROL PLANE IS A CONTAINER, not an in-process tunnel server.
	//
	// frps used to run here in the outpost process while `k3s server` ran
	// elsewhere. That splits the two across network namespaces, and the
	// apiserver reaches a worker's kubelet through a port frps publishes on
	// LOOPBACK — which is per-namespace. The result was a plane that
	// scheduled pods correctly and failed every `kubectl logs` with a 502.
	// Co-locating them in one container is what cloudbox does in one process,
	// and it is the only arrangement that works identically on Linux and on
	// macOS (where podman runs a VM, so "the host" is a different machine).
	kubeDir := controlPlaneKubeconfigDir()
	opts := runtime.ServerOptions{
		AgentName:      fc.ClusterNodeName(),
		TunnelToken:    token,
		STCPSecret:     secret,
		TunnelBindAddr: addr,
		TunnelBindPort: port,
		// Deliberately NOT clusterAPIPort: that is the port this host's
		// visitor binds for the cluster it JOINS. Letting the hosted plane
		// reuse it makes kubectl hit whichever listener won.
		APIPort:       runtime.DefaultControlPlaneAPIPort,
		KubeconfigDir: kubeDir,
		TLSSANs:       fc.Cluster.TunnelSANs(),
	}
	if opts.AgentName == "" {
		slog.Warn("control plane: disabled (no node name)")
		return
	}

	// Remember where the kubeconfig lands so the reconcilers below — and the
	// operator — can find it without being told twice.
	if err := rememberControlPlaneKubeconfig(fc, cfgPath, runtime.KubeconfigPath(kubeDir)); err != nil {
		slog.Warn("control plane: could not record kubeconfig path", "err", err)
	}

	// A1/A3 — supervise the container for the daemon's life instead of a
	// one-shot UpServer. The container's entrypoint exits 1 on any inner
	// failure expecting a supervisor to recreate it, and `podman ps` Up is not
	// evidence the apiserver serves — so the supervisor gates on the apiserver
	// readiness probe (runtime/health.go), not just container state. A boot
	// failure (cold engine) is no longer terminal: the loop backs off and
	// retries, the same way the agent-join side does.
	g.Go(func() error {
		if err := runtime.SuperviseServer(ctx, opts, runtime.SupervisorConfig{}); err != nil && ctx.Err() == nil {
			slog.Warn("control plane: supervisor exited", "err", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		// Stop on shutdown so the published ports free for the re-exec. The
		// DATA VOLUME IS KEPT — removing it would destroy the cluster's
		// identity and invalidate every worker's join token, which a restart
		// is not a request to do.
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = runtime.DownServer(stopCtx, opts)
		return nil
	})

	// The reconcilers act on the plane THIS host hosts, so they belong here
	// beside the server rather than in the join path.
	startControlPlaneReconcilers(ctx, g, fc)
}

// controlPlaneKubeconfigDir is the host directory the control-plane container
// writes its admin kubeconfig into.
func controlPlaneKubeconfigDir() string {
	if d := strings.TrimSpace(os.Getenv("OUTPOST_CONTROL_PLANE_KUBECONFIG_DIR")); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "outpost-control-plane")
	}
	return filepath.Join(os.TempDir(), "outpost-control-plane")
}

// rememberControlPlaneKubeconfig records the path so the reconcilers pick it
// up without the operator having to configure what we already know. An
// explicit operator value is never overwritten.
func rememberControlPlaneKubeconfig(fc *conf.FileConfig, cfgPath, path string) error {
	if fc == nil || fc.Cluster == nil || strings.TrimSpace(fc.Cluster.ControlPlaneKubeconfig) != "" {
		return nil
	}
	fc.Cluster.ControlPlaneKubeconfig = path
	if cfgPath == "" {
		return nil
	}
	return conf.SaveFile(cfgPath, fc)
}

// startControlPlaneReconcilers lives in control_plane_reconcilers.go — it has
// to survive a kubeconfig that the control-plane container has not written yet,
// which is a story of its own.

// clusterReport renders this host's Kubernetes participation for the fleet
// inventory, or nil when it joins no cluster.
//
// This is a REPORT, not a definition — the same report/author split the
// *:registry scopes encode. It rides the fleet-registry push rather than
// getting an endpoint of its own precisely so it cannot be mistaken for a way
// to change cluster membership from the cloud.
//
// The value it carries: placement is now a user choice, so the same host can
// be on a cloudbox-hosted plane today and a peer-hosted one tomorrow. Pushing
// the cluster identity is what makes that move visible in one place instead of
// requiring a walk through every host's config.
func clusterReport(fc *conf.FileConfig, cloudboxBase string) *fleetreg.ClusterInfo {
	if fc == nil || !fc.ClusterOn() {
		return nil
	}
	cc := fc.Cluster

	// THE ROW DESCRIBES THE CLUSTER THIS HOST IS A NODE OF — the JOIN, not
	// whatever plane it may also host. Those were the same thing until
	// hosting and joining became independent, and this function kept reading
	// cc.APIURL (the cloudbox-issued field) after they diverged. The result
	// was two wrong rows: a host that had moved to a peer plane still
	// reported cloudbox, and a host running a plane for others reported
	// "self-hosted" while pointing at cloudbox's endpoint.
	endpoint := cc.APIURL
	if cc.JoinsPeerPlane() {
		// The apiserver is reached through a local tunnel; the loopback port
		// is this host's own artifact, which is exactly why the identity below
		// comes from the token's CA hash rather than from this address.
		endpoint = fmt.Sprintf("https://127.0.0.1:%d", clusterAPIPort(cc))
	}

	// hostsThisCluster is the narrow claim the inventory needs: does this host
	// run the plane named in THIS row? Hosting a plane while joining someone
	// else's must NOT read as "this cluster's control plane" — that would name
	// the wrong host as the plane's owner on the cluster page.
	hostsThisCluster := cc.ControlPlaneOn() && isLoopbackURL(endpoint)

	cpAddr, cpPort := cc.TunnelBind()
	info := &fleetreg.ClusterInfo{
		Placement:    fleetreg.ClassifyPlacement(endpoint, cloudboxBase, hostsThisCluster),
		Endpoint:     endpoint,
		ControlPlane: hostsThisCluster,
		CA:           cc.CA,
		NodeToken:    cc.NodeToken,
	}
	if cc.ControlPlaneOn() {
		info.HostsControlPlane = true
		info.ControlPlaneEndpoint = net.JoinHostPort(cpAddr, strconv.Itoa(cpPort))
	}
	base := fc.ClusterNodeName()
	if cc.HasAgentRuntime() {
		info.Runtimes = append(info.Runtimes, conf.ClusterRuntimeAgent)
		if base != "" {
			// The k3s agent registers as <base>-<node-id>. The base is what an
			// operator matches on — cloudbox's node view already strips that
			// suffix — so reporting the base keeps the two views consistent.
			info.Nodes = append(info.Nodes, base)
		}
	}
	for _, mode := range cc.VirtualRuntimes() {
		info.Runtimes = append(info.Runtimes, mode)
		if base != "" {
			info.Nodes = append(info.Nodes, base+"-"+mode)
		}
	}
	return info
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
	exp := vknode.TokenExpiry(fc.Cluster.Token)
	if exp.IsZero() {
		return true
	}
	return time.Now().After(exp)
}

// persistClusterCredential writes a freshly-fetched (or rotated)
// kubeconfig triple back into fc.Cluster + saves the FileConfig so a
// future outpost restart starts from the rotated state without
// re-fetching. Best-effort: a save failure logs but doesn't undo the
// in-memory rotation (the next refresh tick will try again).
func persistClusterCredential(fc *conf.FileConfig, cfgPath string, p *vknode.ParsedKubeconfig) {
	if fc.Cluster == nil {
		// Defensive: we only reach here for a host already joining
		// (shouldFetchKubeconfig is gated on ClusterOn), so Cluster is
		// normally non-nil with Enabled explicitly true. Mark Enabled so
		// the credential we're persisting isn't left dangling under an
		// opt-in (nil→off) config.
		on := true
		fc.Cluster = &conf.ClusterConfig{Enabled: &on}
	}
	fc.Cluster.APIURL = p.APIURL
	fc.Cluster.Token = p.Token
	fc.Cluster.CA = p.CA
	if cfgPath == "" {
		return
	}
	if err := conf.SaveFile(cfgPath, fc); err != nil {
		slog.Warn("cluster virtual runtimes: rotated credentials but file save failed",
			"err", err, "path", cfgPath)
	}
}

// tryReattach is the boot-time Layer-1 defense call: when we hold a
// valid AccessToken from a previous Exchange, POST it to
// /api/register/reattach so cloudbox can recover any host-row state
// it has lost (deleted row, DB restored from stale backup, schema
// migration accident). Best-effort — failure must NOT prevent boot,
// because the existing host row may still be perfectly valid and a
// transient cloudbox blip shouldn't gate the daemon.
//
// On success, merges cloudbox-controlled fields (MatrixToken,
// RemotePort, cluster join data) into fc and writes the merged
// config to disk. Locally-managed fields (Apps, builtins, networking,
// AdminUsers) are left untouched.
//
// Returns the (possibly-mutated) fc and the error from the call.
// Callers that don't need the error can ignore it.
func tryReattach(ctx context.Context, fc *conf.FileConfig, cfgPath string) (*conf.FileConfig, error) {
	if fc == nil {
		return fc, nil
	}
	if strings.TrimSpace(fc.AccessToken) == "" || strings.TrimSpace(fc.AgentName) == "" {
		return fc, nil
	}
	base := cloudboxHTTPBase(fc)
	if base == "" {
		return fc, nil
	}

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	refreshed, err := portal.Reattach(rctx, portal.ReattachRequest{
		ServerURL:   base,
		AccessToken: fc.AccessToken,
		Name:        fc.AgentName,
		AuthURL:     fc.AuthURL,
		ClientOnly:  fc.ClientOnly,
	})
	if err != nil {
		slog.Warn("reattach: best-effort recovery failed (continuing boot)",
			"err", err, "name", fc.AgentName, "cloudbox", base)
		return fc, err
	}

	// Cloudbox-controlled fields — overwrite. The rest of fc stays as-is.
	changed := false
	if refreshed.Token != "" && refreshed.Token != fc.Token {
		fc.Token = refreshed.Token
		changed = true
	}
	if refreshed.RemotePort != 0 && refreshed.RemotePort != fc.RemotePort {
		fc.RemotePort = refreshed.RemotePort
		changed = true
	}
	if refreshed.ServerAddr != "" && refreshed.ServerAddr != fc.ServerAddr {
		fc.ServerAddr = refreshed.ServerAddr
		changed = true
	}
	if refreshed.ServerPort != 0 && refreshed.ServerPort != fc.ServerPort {
		fc.ServerPort = refreshed.ServerPort
		changed = true
	}
	if refreshed.Protocol != "" && refreshed.Protocol != fc.Protocol {
		fc.Protocol = refreshed.Protocol
		changed = true
	}
	// THE PEER-TICKET PUBKEY MUST BE MERGED HERE, NOT ONLY AT FIRST PAIRING.
	//
	// portal.Reattach decodes `cloudbox_ticket_pubkey` on every reattach —
	// its own doc comment says it is re-published "so a re-pair after a
	// cloudbox key rotation picks up the new key without an explicit
	// migration step". That promise was not kept: this merge dropped the
	// field on the floor, so the value could only ever be written by the
	// FIRST-PAIRING path in portal.Exchange.
	//
	// The consequence is silent and total. Any host paired BEFORE cloudbox
	// had a ticket signer keeps an empty pubkey forever, no matter how many
	// times it reattaches. sshHandler gates the whole peer-ticket branch on
	// `len(deps.TicketPubkey) > 0`, so an empty key means the ticket is never
	// even examined: cloudboxVouched stays false, NoClientAuth is never set,
	// and a LAN-direct or mesh-direct client that legitimately holds a valid
	// ticket is rejected with `ssh: unable to authenticate, attempted methods
	// [none]`. That error names the CLIENT, which is why it reads as a client
	// bug and cost real time to localize.
	//
	// Overwrite-if-non-empty (not merge-if-absent): a rotation must be able to
	// REPLACE an existing key, which is the case the doc comment describes.
	// An empty value from cloudbox is ignored so a signer-less deployment
	// cannot erase a working key.
	if refreshed.CloudboxTicketPubkey != "" &&
		refreshed.CloudboxTicketPubkey != fc.CloudboxTicketPubkey {
		fc.CloudboxTicketPubkey = refreshed.CloudboxTicketPubkey
		changed = true
	}
	if refreshed.Cluster != nil {
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		// CLUSTER MEMBERSHIP BELONGS TO WHICHEVER PLANE THIS HOST JOINED.
		// When that is a peer's, cloudbox's node token, STCP secret, ports
		// and overlay keys describe a DIFFERENT cluster, and applying them
		// swaps the node's credentials out from under the operator on the
		// next boot.
		//
		// This is not hypothetical: it is what happened the first time a host
		// was pointed at a peer plane. The tunnel connected and the agent
		// reached the right apiserver, but the reattach had already replaced
		// the peer's node token with cloudbox's, so k3s rejected the join
		// with a CA-hash mismatch — an error that reads like a broken tunnel
		// while the tunnel was in fact working.
		//
		// The cloudbox PAIRING fields above (Token, ServerAddr, Protocol, …)
		// are deliberately still refreshed: staying paired with cloudbox for
		// apps, shell and the LLM pool is independent of which cluster this
		// host is a node of.
		if reconcileClusterMembershipOnReattach(fc, refreshed) {
			changed = true
		}
	}

	if changed && cfgPath != "" {
		if err := conf.SaveFile(cfgPath, fc); err != nil {
			slog.Warn("reattach: succeeded but SaveFile failed",
				"err", err, "path", cfgPath)
		} else {
			slog.Info("reattach: refreshed cloudbox-side state",
				"name", fc.AgentName, "cloudbox", base)
		}
	} else {
		slog.Info("reattach: no-op (cloudbox-side state already in sync)",
			"name", fc.AgentName, "cloudbox", base)
	}
	return fc, nil
}

// applyCloudboxClusterMembership copies the cluster-membership fields cloudbox
// owns onto fc, reporting whether anything changed. Split out so the
// peer-plane guard above is one readable branch rather than a condition
// repeated on every field.
func applyCloudboxClusterMembership(fc, refreshed *conf.FileConfig) bool {
	changed := captureCloudSTCPSecret(fc.Cluster, refreshed.Cluster)
	{
		if refreshed.Cluster.NodeToken != "" {
			if fc.Cluster.NodeToken != refreshed.Cluster.NodeToken {
				changed = true
			}
			fc.Cluster.NodeToken = refreshed.Cluster.NodeToken
		}
		if refreshed.Cluster.STCPSecret != "" {
			if fc.Cluster.STCPSecret != refreshed.Cluster.STCPSecret {
				changed = true
			}
			fc.Cluster.STCPSecret = refreshed.Cluster.STCPSecret
		}
		if refreshed.Cluster.K8sAPIPort != 0 {
			if fc.Cluster.K8sAPIPort != refreshed.Cluster.K8sAPIPort {
				changed = true
			}
			fc.Cluster.K8sAPIPort = refreshed.Cluster.K8sAPIPort
		}
		if refreshed.Cluster.KubeletProxyPort != 0 {
			if fc.Cluster.KubeletProxyPort != refreshed.Cluster.KubeletProxyPort {
				changed = true
			}
			fc.Cluster.KubeletProxyPort = refreshed.Cluster.KubeletProxyPort
		}
		if refreshed.Cluster.OverlayLoginServer != "" {
			if fc.Cluster.OverlayLoginServer != refreshed.Cluster.OverlayLoginServer {
				changed = true
			}
			fc.Cluster.OverlayLoginServer = refreshed.Cluster.OverlayLoginServer
		}
		if refreshed.Cluster.OverlayAuthKey != "" {
			fc.Cluster.OverlayAuthKey = refreshed.Cluster.OverlayAuthKey
			changed = true
		}
		if refreshed.Cluster.OverlayPodCIDR != "" {
			if fc.Cluster.OverlayPodCIDR != refreshed.Cluster.OverlayPodCIDR {
				changed = true
			}
			fc.Cluster.OverlayPodCIDR = refreshed.Cluster.OverlayPodCIDR
		}
	}
	return changed
}

// reconcileClusterMembershipOnReattach applies cloudbox's refreshed cluster
// membership to fc during a boot reattach, honoring the peer-plane guard.
// Reports whether fc changed.
//
// Cloudbox re-issues cluster membership (node token, STCP secret, api/kubelet
// ports, overlay trio) on every reattach, and those fields describe the
// CLOUDBOX plane. When this host is a node of a PEER plane instead:
//
//   - node token / STCP secret: applying cloudbox's would swap the peer's
//     credentials for a different cluster's, failing the join with a CA-hash
//     mismatch that reads like a broken tunnel (commit a69e7a5). So the refresh
//     is skipped — the peer's own credentials are left untouched.
//   - overlay trio (B6): skipping the refresh is NOT enough. A host that MOVED
//     from the cloudbox plane to a peer plane still carries the cloud plane's
//     overlay login server / auth key / pod CIDR on disk, and the runtime joins
//     an overlay purely on OverlayLoginServer being non-empty — so a stale trio
//     attaches this peer-plane node to the CLOUD overlay while its k3s agent
//     joins the peer plane, a split-brain the node-token guard left behind. The
//     peer-join flow (admincore.JoinPeerPlane) never sets these three, so
//     clearing them can only remove cloud leftovers, never a peer's own overlay.
func reconcileClusterMembershipOnReattach(fc, refreshed *conf.FileConfig) bool {
	if fc.Cluster.JoinsPeerPlane() {
		cleared := clearCloudOverlayCreds(fc.Cluster)
		// The one cloudbox-plane value a peer-joined worker still needs:
		// cloudbox's own STCP secret, which authorizes the overlay-control
		// visitor its runtime container opens DIRECTLY against cloudbox to
		// register on the tailnet (the ts2021 hop cannot ride the public
		// HTTPS URL). Captured into its own field — never onto STCPSecret,
		// which holds the PEER's credential here.
		captured := captureCloudSTCPSecret(fc.Cluster, refreshed.Cluster)
		slog.Debug("reattach: host joins a peer control plane; keeping its cluster credentials and dropping any stale cloud overlay creds",
			"join_endpoint", fc.Cluster.JoinEndpoint, "cleared_cloud_overlay", cleared,
			"captured_cloud_stcp_secret", captured)
		return cleared || captured
	}
	return applyCloudboxClusterMembership(fc, refreshed)
}

// captureCloudSTCPSecret records cloudbox's cluster STCP secret in its
// dedicated field, reporting whether it changed. Safe on every plane:
// on a plain cloudbox-plane member it simply mirrors STCPSecret; on a
// peer-joined worker or a control-plane host it is the ONLY place the
// cloudbox secret survives, because those overwrite STCPSecret with
// their own credential. See conf.ClusterConfig.CloudSTCPSecret.
func captureCloudSTCPSecret(cc, refreshed *conf.ClusterConfig) bool {
	if cc == nil || refreshed == nil {
		return false
	}
	secret := strings.TrimSpace(refreshed.STCPSecret)
	if secret == "" || cc.CloudSTCPSecret == secret {
		return false
	}
	cc.CloudSTCPSecret = secret
	return true
}

// clearCloudOverlayCreds drops the cloudbox overlay credentials from cc,
// reporting whether anything was cleared. See B6 rationale on
// reconcileClusterMembershipOnReattach.
func clearCloudOverlayCreds(cc *conf.ClusterConfig) bool {
	if cc == nil {
		return false
	}
	changed := cc.OverlayLoginServer != "" ||
		cc.OverlayAuthKey != "" ||
		cc.OverlayPodCIDR != ""
	cc.OverlayLoginServer = ""
	cc.OverlayAuthKey = ""
	cc.OverlayPodCIDR = ""
	return changed
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

// appsAsTransient bridges *agent.AppRegistry to vknode.TransientApps.
// Each transient pod registration uses RequireLogin: false because
// Cluster service auth happens at cloudbox's /api/cluster/svc/* entry
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
			if ac.Enabled && ac.TrustCloudIdentity && strings.TrimSpace(ac.SSOSecret) == "" {
				return nil, fmt.Errorf("app %q: trust_cloud_identity requires a non-empty sso_secret", ac.Name)
			}
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

func effectiveBashyServices(fc *conf.FileConfig) []conf.BashyService {
	byName := map[string]conf.BashyService{}
	for _, svc := range conf.DefaultBashyServices() {
		byName[svc.Name] = svc
	}
	if fc != nil {
		hasAppsServiceOverride := false
		for _, svc := range fc.BashyServices {
			if strings.TrimSpace(svc.Name) == "" {
				continue
			}
			if svc.Name == "apps" {
				hasAppsServiceOverride = true
			}
			if svc.AppName == "" {
				svc.AppName = svc.Name
			}
			if def, ok := byName[svc.Name]; ok {
				if !svc.EnabledSet && !svc.Enabled {
					svc.Enabled = def.Enabled
				}
				if svc.AppPort == 0 {
					svc.AppPort = def.AppPort
				}
				if svc.AppName == svc.Name && def.AppName != "" {
					svc.AppName = def.AppName
				}
				if def.RequireLogin {
					svc.RequireLogin = true
				}
				if def.MeshService != "" && svc.MeshService == "" {
					svc.MeshService = def.MeshService
				}
			}
			// Inherit the default Command base (e.g. sdlc → ["sdlc","service"]) so
			// an operator can enable a service without re-declaring its argv.
			if len(svc.Command) == 0 {
				if def, ok := byName[svc.Name]; ok {
					svc.Command = def.Command
				}
			}
			if len(svc.Args) == 0 {
				if def, ok := byName[svc.Name]; ok {
					svc.Args = append([]string{}, def.Args...)
				}
			}
			// Security properties required by a built-in are not optional
			// defaults. In particular, meet's coopauth guard requires the
			// signed identity contract even when the operator enables it through
			// a terse bashy_services override.
			if def, ok := byName[svc.Name]; ok && def.TrustCloudIdentity {
				svc.TrustCloudIdentity = true
			}
			if def, ok := byName[svc.Name]; ok && def.ElevationRequired {
				svc.ElevationRequired = true
			}
			byName[svc.Name] = svc
		}
		if fc.BashyServices != nil && !hasAppsServiceOverride && fc.BashyAppsEnabled == nil {
			svc := byName["apps"]
			svc.Enabled = false
			byName["apps"] = svc
		}
		if fc.BashyAppsEnabled != nil {
			svc := byName["apps"]
			svc.Name = "apps"
			svc.Enabled = *fc.BashyAppsEnabled
			svc.Command = []string{"apps", "service"}
			svc.AppName = "apps"
			svc.AppPort = fc.BashyAppsPortOrDefault()
			svc.RequireLogin = true
			svc.ElevationRequired = true
			svc.TrustCloudIdentity = true
			byName["apps"] = svc
		} else if svc := byName["apps"]; svc.Enabled && svc.AppPort == conf.DefaultBashyAppsPort && fc.BashyAppsPort > 0 {
			svc.AppPort = fc.BashyAppsPort
			byName["apps"] = svc
		}
		if fc.LoomOn() {
			svc := byName["loom"]
			svc.Name = "loom"
			svc.Enabled = true
			svc.AppName = "loom"
			svc.AppPort = fc.LoomPortOrDefault()
			svc.RequireLogin = true
			svc.MeshService = "git"
			byName["loom"] = svc
		}
		if fc.MeetOn() {
			svc := byName["meet"]
			svc.Name = "meet"
			svc.Enabled = true
			svc.AppName = "meet"
			svc.AppPort = fc.MeetPortOrDefault()
			svc.RequireLogin = true
			svc.TrustCloudIdentity = true
			byName["meet"] = svc
		}
	}
	out := make([]conf.BashyService, 0, len(byName))
	for _, svc := range byName {
		out = append(out, svc)
	}
	return out
}

func registerBashyServiceApps(fc *conf.FileConfig, reg *agent.AppRegistry) error {
	if reg == nil {
		return nil
	}
	for _, svc := range effectiveBashyServices(fc) {
		if !svc.Enabled || svc.AppPort <= 0 {
			continue
		}
		name := svc.AppName
		if name == "" {
			name = svc.Name
		}
		if svc.TrustCloudIdentity && strings.TrimSpace(svc.SSOSecret) == "" {
			return fmt.Errorf("bashy service app %q: trust_cloud_identity requires a non-empty sso_secret", name)
		}
		if err := reg.RegisterWithMeta(name, fmt.Sprintf("http://127.0.0.1:%d", svc.AppPort), agent.AppMeta{
			RequireLogin:       svc.RequireLogin,
			ElevationRequired:  svc.ElevationRequired,
			TrustCloudIdentity: svc.TrustCloudIdentity,
			SSOSecret:          svc.SSOSecret,
		}); err != nil {
			return err
		}
	}
	return nil
}

func startBashyServiceSupervisors(g *errgroup.Group, ctx context.Context, fc *conf.FileConfig, meshHost *mesh.Host) {
	if g == nil {
		return
	}
	// Pin (or leave at latest) the bashy release the self-heal auto-install
	// fetches when bashy is missing. Read once at boot; changing it schedules
	// a restart, which re-enters here.
	if fc != nil {
		bashyResolver.SetVersion(fc.BashyVersion)
	}
	// Fleet auto-roll: reconcile an existing outpost-managed bashy to the matched
	// version at boot, even on hosts with no enabled bashy service (they may still
	// run bashy for deploy jobs). Best-effort; never blocks startup.
	g.Go(func() error {
		bashyResolver.ReconcileExisting(ctx)
		return nil
	})
	for _, svc := range effectiveBashyServices(fc) {
		if !svc.Enabled {
			continue
		}
		svc := svc
		g.Go(func() error {
			return superviseBashyService(ctx, fc, svc, meshHost)
		})
	}
}

func superviseBashyService(ctx context.Context, fc *conf.FileConfig, svc conf.BashyService, meshHost *mesh.Host) error {
	if svc.Name == "" {
		return nil
	}
	addr := ""
	if svc.AppPort > 0 {
		addr = fmt.Sprintf("127.0.0.1:%d", svc.AppPort)
		if meshHost != nil && svc.MeshService != "" {
			meshHost.Forwarder().Expose(svc.MeshService, addr)
			slog.Info("bashy service exposed over mesh", "service", svc.Name, "mesh_service", svc.MeshService, "addr", addr)
		}
	}
	if err := startBashyService(ctx, fc, svc); err != nil {
		slog.Warn("bashy service: start failed", "service", svc.Name, "err", err)
	}
	ticker := time.NewTicker(bashyServicePollInterval)
	defer ticker.Stop()
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := runBashyServiceCommand(stopCtx, svc, "stop", nil); err != nil {
			slog.Warn("bashy service: stop failed", "service", svc.Name, "err", err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			ok, err := bashyServiceRunning(ctx, svc)
			if err != nil || !ok {
				if err := startBashyService(ctx, fc, svc); err != nil {
					slog.Warn("bashy service: restart failed", "service", svc.Name, "err", err)
				}
			}
		}
	}
}

var bashyServicePollInterval = 30 * time.Second

// bashyAppsLANBind resolves the exact private interface address used for the
// direct LAN listener. It is a seam for deterministic tests. Binding an exact
// RFC1918 address avoids accidentally publishing the console on a public
// interface, while bashy Apps itself keeps the loopback listener used by the
// Cloudbox tunnel.
var bashyAppsLANBind = discoverPrivateLANIPv4

func discoverPrivateLANIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	return firstPrivateLANIPv4(addrs)
}

func firstPrivateLANIPv4(addrs []net.Addr) string {
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || ip == nil || ip.To4() == nil || ip.IsLoopback() || !ip.IsPrivate() {
			continue
		}
		return ip.String()
	}
	return ""
}

func startBashyService(ctx context.Context, fc *conf.FileConfig, svc conf.BashyService) error {
	args := append([]string{}, svc.Args...)
	if svc.Name != "apps" && svc.RootURL != "" {
		args = append(args, "--root-url", svc.RootURL)
	} else if svc.Name == "loom" {
		if root := bashyServiceCloudboxRoot(fc, svc); root != "" {
			args = append(args, "--root-url", root)
		}
	}
	if svc.Name == "loom" && svc.AppPort > 0 && svc.AppPort != 31880 {
		args = append(args, "--port", strconv.Itoa(svc.AppPort))
	}
	if svc.Name == "apps" && svc.AppPort > 0 {
		args = append(args, "--port", strconv.Itoa(svc.AppPort))
		if bind := bashyAppsLANBind(); bind != "" {
			args = append(args, "--bind", bind)
		} else {
			slog.Warn("bashy Apps: no private LAN IPv4; keeping Cloudbox/loopback access only")
		}
	}
	return runBashyServiceCommand(ctx, svc, "start", args)
}

func bashyServiceRunning(ctx context.Context, svc conf.BashyService) (bool, error) {
	out, err := outputBashyServiceCommand(ctx, svc, "status", nil)
	if err != nil {
		return false, err
	}
	text := strings.ToLower(string(out))
	return !strings.Contains(text, "stopped") && !strings.Contains(text, "not running"), nil
}

func runBashyServiceCommand(ctx context.Context, svc conf.BashyService, verb string, extra []string) error {
	_, err := outputBashyServiceCommand(ctx, svc, verb, extra)
	return err
}

func outputBashyServiceCommand(ctx context.Context, svc conf.BashyService, verb string, extra []string) ([]byte, error) {
	// Resolve (and, if missing, self-heal by auto-installing) the bashy binary
	// rather than trusting it to be on the daemon's PATH. A launchd/systemd
	// daemon has a narrow PATH, and a host may simply not have bashy yet.
	bin, err := bashyResolver.Path(ctx)
	if err != nil {
		return nil, err
	}
	// The base argv is svc.Command when set (e.g. ["sdlc","service"] →
	// `bashy sdlc service start`), else just the service name (`bashy loom start`).
	base := svc.Command
	if len(base) == 0 {
		base = []string{svc.Name}
	}
	args := append([]string{}, base...)
	args = append(args, verb)
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, bin, args...)
	// Inject the host's cloudbox-vault secrets (rendered through the local
	// binding template) into the long-running service process, so a service that
	// needs GITHUB_TOKEN etc. gets it with no human step. Only at start — the
	// verb that launches the daemon; status/stop are quick control calls polled
	// every 30s and must not hit cloudbox each time. The child stays decoupled —
	// it just reads env vars.
	if verb == "start" {
		// Join the existing OTel telemetry plane under this service's OWN
		// service.name (loom, …) — not "outpost" — so its deploy activity is
		// filterable when an agent supervises a deployment via the observability
		// backend. No-op unless an OTLP endpoint is configured on the host.
		extraEnv := telemetry.ChildEnv(svc.Name)
		if svc.SecretsEnvOn() {
			extraEnv = append(extraEnv, bashyServiceSecretsEnv(ctx, bin)...)
		}
		if len(extraEnv) > 0 {
			cmd.Env = append(os.Environ(), extraEnv...)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("bashy service command %q failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// bashyServiceSecretsEnv runs `bashy secrets env` and returns "NAME=value"
// entries for injection into a supervised service. Best-effort: any failure
// (offline, no secrets token, no binding template) yields nil and the service
// simply starts without injected secrets — matching `secrets env`'s own
// never-break-startup contract. `.Output()` (stdout only) keeps stderr warnings
// like "cloudbox unreachable; using cache" out of the parsed set.
func bashyServiceSecretsEnv(ctx context.Context, bin string) []string {
	out, err := exec.CommandContext(ctx, bin, "secrets", "env").Output()
	if err != nil {
		return nil
	}
	m := secrets.ParseEnv(out)
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	return env
}

func bashyServiceCloudboxRoot(fc *conf.FileConfig, svc conf.BashyService) string {
	base := cloudboxHTTPBase(fc)
	if base == "" || fc == nil || fc.AgentName == "" {
		return ""
	}
	appName := svc.AppName
	if appName == "" {
		appName = svc.Name
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/matrix/h/" + url.PathEscape(fc.AgentName) + "/app/" + url.PathEscape(appName) + "/"
	return u.String()
}

func registerCmd() *cobra.Command {
	var (
		serverURL    string
		code         string
		recoveryCode string
		name         string
		out          string
		authURL      string
		title        string
		assumeYes    bool
		clientOnly   bool
		ring         string
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
			// Recovery branch (Layer-4 defense): when --recovery-code
			// is set, skip the invite-redemption Exchange path entirely
			// and call /api/register/recover instead. The recovery
			// code IS the credential (no invite needed).
			if recoveryCode != "" {
				if serverURL == "" {
					serverURL = defaultPortal
				}
				if name == "" {
					name = defaultHostName()
				}
				if name == "" {
					return errors.New("--recovery-code requires --name (couldn't auto-detect hostname)")
				}
				return doRecover(cmd.Context(), serverURL, recoveryCode, name, title, authURL, out, clientOnly)
			}

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

				if err := doExchange(cmd.Context(), serverURL, code, name, title, authURL, out, clientOnly, ring); err == nil {
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

			// A daemon may already be running unpaired (the installer
			// brought it up, the operator pairs later). It holds the
			// pre-pairing config in memory and owns the pidfile, so a plain
			// `outpost start` / execSelfStart would refuse to boot over it.
			// Signal it to restart through MCP — under a supervisor it exits
			// and gets respawned, standalone it self-re-execs — so it
			// re-reads the freshly-merged config and brings up the tunnel.
			// The merge above preserved its MCP bearer token, so the call
			// authenticates. handled=true means a daemon was present, so we
			// skip our own start-now prompt.
			if handled, _ := restartRunningDaemon(cmd.Context()); handled {
				return nil
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
	cmd.Flags().StringVar(&recoveryCode, "recovery-code", "",
		"Out-of-band recovery code minted at first Exchange. When set, hits /api/register/recover instead of the invite path — used when BOTH the agent.json AccessToken AND the matrix tunnel are unrecoverable. Pair with --name (the host name this outpost was originally paired as).")
	cmd.Flags().StringVar(&name, "name", "", "Host name to display in the portal (default: this machine's hostname)")
	cmd.Flags().StringVar(&out, "out", "", "Output config path (default: the OS-standard user-config path)")
	cmd.Flags().StringVar(&authURL, "auth-url", "",
		"Optional application-level auth endpoint. When set, the agent forwards {user,password} to it and trusts the returned role; the host OS is no longer consulted.")
	cmd.Flags().StringVar(&title, "title", "",
		"Human-readable subtitle shown in the portal (e.g. \"Family streaming box\"). Required when --auth-url is set; optional otherwise (falls back to the OS user / hostname).")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "On success, start outpost immediately without asking")
	cmd.Flags().BoolVar(&clientOnly, "client-only", false,
		"Pair this machine as a credential-only outpost — outbound SSH via `outpost ssh-proxy` only, no inbound listeners, no matrix tunnel. The host row shows up in cloudbox with a 'client' badge so the operator can see it; it cannot be a share target.")
	cmd.Flags().StringVar(&ring, "ring", "",
		"Optional deployment ring tag (e.g. dev/test/stage/prod) that seeds the host's ring on cloudbox at first pairing. Used to scope fleet-upgrade fan-out to one cohort. Empty leaves the host untagged. Cloudbox admins can re-assign rings from the SPA — a subsequent re-pair without --ring will not overwrite their value.")
	return cmd
}

// defaultHostName returns the system hostname with the macOS/mDNS
// `.local` and the older `.lan` suffix stripped, so a Mac that reports
// "host-a.local" pairs as just "host-a" (matching how users typically
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

// doRecover runs the out-of-band recovery flow and writes the
// resulting config to disk. Thin wrapper over portal.Recover.
// Caller side-effect: when successful, the on-disk recovery_code.txt
// is rotated to the new code cloudbox returned (handled inside
// portal.Recover so the same stash format applies as Exchange).
func doRecover(ctx context.Context, serverURL, recoveryCode, name, title, authURL, out string, clientOnly bool) error {
	fc, err := portal.Recover(ctx, portal.RecoverRequest{
		ServerURL:    serverURL,
		Name:         name,
		RecoveryCode: recoveryCode,
		Title:        title,
		AuthURL:      authURL,
		ClientOnly:   clientOnly,
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
	merged := mergePairing(path, fc)
	if err := conf.SaveFile(path, merged); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("Recovered as %q. Config saved to %s\n", merged.AgentName, path)
	return nil
}

// doExchange runs the pairing exchange and writes the resulting config to
// disk. Thin wrapper over portal.Exchange — the admin UI calls the same
// portal package directly so it can layer Apps + toggles in before saving.
func doExchange(ctx context.Context, serverURL, code, name, title, authURL, out string, clientOnly bool, ring string) error {
	fc, err := portal.Exchange(ctx, portal.ExchangeRequest{
		ServerURL:  serverURL,
		Code:       code,
		Name:       name,
		Title:      title,
		AuthURL:    authURL,
		ClientOnly: clientOnly,
		Ring:       ring,
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
	merged := mergePairing(path, fc)
	if err := conf.SaveFile(path, merged); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("Registered as %q. Config saved to %s\n", merged.AgentName, path)
	return nil
}

// mergePairing overlays the portal-controlled fields from a fresh
// exchange/recovery result onto the existing on-disk config at path,
// preserving everything the operator or the running daemon owns: the SPA
// session key and MCP bearer token (clobbering those breaks a running
// daemon's auth), custom apps, outbound mounts, built-in toggles,
// networking binds, and admin_users. Mirrors admincore.Pair so the
// `register` CLI and the admin-UI / MCP pair path converge on identical
// on-disk results instead of the CLI wholesale-overwriting the file.
func mergePairing(path string, exchanged *conf.FileConfig) *conf.FileConfig {
	merged := &conf.FileConfig{}
	if existing, err := conf.LoadFile(path); err == nil && existing != nil {
		merged = existing
	}
	merged.AgentName = exchanged.AgentName
	merged.ServerAddr = exchanged.ServerAddr
	merged.ServerPort = exchanged.ServerPort
	merged.Protocol = exchanged.Protocol
	merged.Token = exchanged.Token
	merged.RemotePort = exchanged.RemotePort
	merged.AuthURL = exchanged.AuthURL
	merged.AccessToken = exchanged.AccessToken
	merged.ClientOnly = exchanged.ClientOnly

	// Cloudbox issues fresh cluster-join credentials at every pairing (node
	// token / STCP secret / ports / overlay + observability endpoints). Carry
	// those forward but keep every HOST-authoritative cluster field.
	//
	// INVERTED MERGE, and why. The previous shape started from cloudbox's block
	// and hand-listed the survivors (Enabled / Runtimes / APIURL / Token / CA /
	// NodeName). That whitelist silently DROPPED any host field not on it — and
	// it did: Cluster.ControlPlane (which booted a control-plane host as a
	// worker, story #12) and Cluster.ControlPlaneKubeconfig (F16: the value
	// never survived a re-pair, so status surfaces could not show it) were both
	// dropped the moment they were added, as were the join_endpoint pair and the
	// tunnel/api addrs. So start from the ON-DISK block instead and overlay ONLY
	// the fields cloudbox owns: every host field — including any added later —
	// is preserved by DEFAULT rather than by remembering to extend a list.
	if exchanged.Cluster != nil {
		if merged.Cluster == nil {
			// Fresh host: nothing on disk to preserve, take cloudbox's block.
			merged.Cluster = exchanged.Cluster
		} else {
			cc := merged.Cluster
			// Cluster MEMBERSHIP (node token, STCP secret, ports, overlay trio)
			// and the fleet observability endpoints describe whichever plane
			// this host is a NODE of. A peer-plane member and a self-hosted
			// control plane mint those themselves; applying cloudbox's would
			// swap them for a different cluster's — the same class of bug as the
			// reattach guard (B6 / a69e7a5). So the cloudbox refresh applies only
			// to a plain cloudbox-plane member. A host that carries its own
			// control-plane creds (control_plane true) or joins a peer keeps them.
			if !cc.JoinsPeerPlane() && !cc.ControlPlaneOn() {
				applyCloudboxClusterMembership(merged, exchanged)
				if v := exchanged.Cluster.MetricsRemoteURL; v != "" {
					cc.MetricsRemoteURL = v
				}
				if v := exchanged.Cluster.LogsRemoteURL; v != "" {
					cc.LogsRemoteURL = v
				}
				if v := exchanged.Cluster.TracesRemoteURL; v != "" {
					cc.TracesRemoteURL = v
				}
			} else if cc.JoinsPeerPlane() {
				// B6: same invariant as the boot reattach — a peer-plane
				// member never keeps the cloud plane's overlay trio past a
				// cloudbox sync point. See reconcileClusterMembershipOnReattach.
				clearCloudOverlayCreds(cc)
				// …but DO keep cloudbox's own STCP secret (dedicated field):
				// it authorizes the overlay-control relay the peer-flannel
				// runtime needs to register on cloudbox's tailnet.
				captureCloudSTCPSecret(cc, exchanged.Cluster)
			} else {
				// Control-plane host: cloudbox membership is skipped, but the
				// relay secret is still captured for the day this host also
				// joins a plane (its own or a peer's) as a worker.
				captureCloudSTCPSecret(cc, exchanged.Cluster)
			}
		}
	}
	return merged
}

// runningDaemonPID returns the pid recorded in the daemon pidfile when
// that process is currently alive, else (0, false).
func runningDaemonPID() (int, bool) {
	p, err := pidFilePath()
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || !processAlive(pid) {
		return 0, false
	}
	return pid, true
}

// restartRunningDaemon triggers a clean restart of an already-running
// local daemon (via the MCP outpost_restart tool) so it re-reads the
// freshly-merged config. Returns handled=true when a daemon was detected —
// the register flow then skips its own start-now prompt / execSelfStart,
// which would otherwise collide on the pidfile. A best-effort MCP failure
// is reported but still counts as handled: the operator can re-apply with
// `outpost restart`, and we must not execSelfStart over the live daemon.
func restartRunningDaemon(ctx context.Context) (bool, error) {
	pid, ok := runningDaemonPID()
	if !ok {
		return false, nil
	}
	session, err := dialMCP(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"Paired, but the running daemon (pid %d) is unreachable over MCP (%v).\nRun `outpost restart` to apply the new pairing.\n", pid, err)
		return true, err
	}
	defer session.close()
	if err := session.callTool(ctx, "outpost_restart", map[string]any{}, nil); err != nil {
		fmt.Fprintf(os.Stderr,
			"Paired, but triggering a restart of the running daemon (pid %d) failed: %v.\nRun `outpost restart` to apply the new pairing.\n", pid, err)
		return true, err
	}
	fmt.Println("Paired. Restarting the running outpost to apply — poll `outpost status` until it reports configured.")
	return true, nil
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
	// Under the supervisor (`outpost supervisord`), don't self-re-exec —
	// just exit, and let the parent respawn us (with a freshly-swapped
	// binary on the upgrade path). The callers already cleared the pidfile,
	// so the respawned daemon claims it cleanly. This is the seam the
	// future blue/green upgrade slots into. Standalone (no supervisor)
	// keeps the detached self-re-exec below.
	if os.Getenv(envSupervised) == "1" {
		slog.Info("outpost: supervised — exiting for the supervisor to respawn")
		os.Exit(0)
	}
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

	buildChild := func() *exec.Cmd {
		c := exec.Command(self, "start")
		// Background daemon: no controlling terminal, no stdin, log->file.
		c.Stdin = nil
		c.Stdout = logF
		c.Stderr = logF
		return c
	}

	c := buildChild()
	detach(c) // platform-specific: Setsid on unix; detached + break-away-from-job on Windows.
	if err := c.Start(); err != nil {
		// Windows only: the parent's Task Scheduler job object may forbid
		// CREATE_BREAKAWAY_FROM_JOB (no JOB_OBJECT_LIMIT_BREAKAWAY_OK), so
		// CreateProcess fails. Re-spawn WITHOUT breakaway from a FRESH Cmd (an
		// already-Started one can't be reused). detachWithoutBreakaway returns
		// false on unix, so this retry only runs where it can actually help.
		retry := buildChild()
		if detachWithoutBreakaway(retry) {
			if rerr := retry.Start(); rerr != nil {
				return fmt.Errorf("start outpost (no-breakaway retry): %w", rerr)
			}
			c = retry
		} else {
			return fmt.Errorf("start outpost: %w", err)
		}
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

	// The parent MUST terminate NOW, deterministically. Returning nil and
	// letting the call stack unwind to process exit was observed on Windows to
	// leave the parent process ALIVE — an orphan daemon that keeps holding its
	// matrix-tunnel session on cloudbox. The freshly-spawned daemon then can't
	// bind its proxy RemotePort ("[<host>-http] start error: port unavailable",
	// looping forever), so the host goes dark despite a valid token. A single
	// restart/upgrade could accrete a 3-day-old orphan this way. os.Exit is the
	// only cross-platform-deterministic termination; the deferred logF.Close()
	// is redundant here (the child dup'd its own handle at Start(), and the OS
	// reclaims ours on exit). The pidfile is already the new daemon's — the old
	// (removePidFile'd) parent leaving is exactly the point.
	logF.Close()
	os.Exit(0)
	return nil // unreachable; keeps the signature honest
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
		Short: "Stop the running outpost daemon (SIGTERM, then SIGKILL after 5s)",
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
			if err := requestStop(proc, pid); err != nil {
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
			// Graceful stop ignored — escalate.
			_ = forceStop(proc, pid)
			time.Sleep(200 * time.Millisecond)
			_ = os.Remove(p)
			fmt.Printf("Force-killed outpost (pid %d) after SIGTERM timeout.\n", pid)
			return nil
		},
	}
}

// clusterSourceAdapter bridges a clusterllm.Detector to the ollama
// watcher's ClusterSource interface, mapping a detected backend onto the
// registry-push ClusterCapacity. Returns nil unless a backend is actually
// running, so a configured-but-down endpoint (or a single-machine
// outpost) keeps the push byte-identical to the no-cluster shape. Uses a
// background context — the detector bounds its own probes and caches the
// result for a TTL, so the per-tick call is cheap.
type clusterSourceAdapter struct{ d *clusterllm.Detector }

func (a clusterSourceAdapter) ClusterSnapshot() *ollama.ClusterCapacity {
	if a.d == nil {
		return nil
	}
	info := a.d.Info(context.Background())
	if info.State != clusterllm.StateRunning {
		return nil
	}
	return &ollama.ClusterCapacity{
		MaxModelBytes: info.AggregateVRAMBytes,
		MemberCount:   info.MemberCount,
		Backend:       info.Backend,
	}
}

func (a clusterSourceAdapter) ClusterModels() []ollama.ModelInfo {
	if a.d == nil {
		return nil
	}
	info := a.d.Info(context.Background())
	if info.State != clusterllm.StateRunning || len(info.Models) == 0 {
		return nil
	}
	models := make([]ollama.ModelInfo, 0, len(info.Models))
	for _, m := range info.Models {
		models = append(models, ollama.ModelInfo{
			Name: m.Name,
			Size: m.Size,
		})
	}
	return models
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns false if ctx was
// cancelled (so callers can exit their retry loop), true if the full duration
// elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// minDur returns the smaller of two durations.
func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// loadVaultSecretsEnv resolves the operator's cloudbox-vault secrets into
// KEY=VALUE env entries by running `bashy secrets env` (which reads the vault via
// the file-based secrets token, so it works from the daemon). These are injected
// into the act_runner daemon env so CI jobs (e.g. a deploy step pushing to
// GitHub) inherit GITHUB_TOKEN and friends WITHOUT a per-repo secret — the token
// stays in the vault. Best-effort: any error yields no entries. `bashy secrets
// env` emits shell `export KEY='value'` lines (single-quoted); we unquote them.
func loadVaultSecretsEnv(ctx context.Context) []string {
	bin, err := bashyResolver.Path(ctx)
	if err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, bin, "secrets", "env").Output()
	if err != nil {
		slog.Warn("act_runner: could not load vault secrets (bashy secrets env) — CI deploy steps needing GITHUB_TOKEN may fail", "err", err)
		return nil
	}
	var env []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key, val := line[:eq], line[eq+1:]
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			val = strings.ReplaceAll(val[1:len(val)-1], `'\''`, `'`) // shell single-quote unescape
		}
		env = append(env, key+"="+val)
	}
	return env
}

// resolveActrunnerDockerHost returns the DOCKER_HOST the tier-3 sandbox executor
// dials to reach the container runtime. An explicit actrunner_docker_host wins;
// otherwise it asks bashy podman for its host-side machine socket. Returns ""
// when it can't resolve — host-lane (runs-on: host) jobs still run; only
// runs-on: sandbox needs a container runtime.
func resolveActrunnerDockerHost(ctx context.Context, fc *conf.FileConfig) string {
	if fc.ActrunnerDockerHost != "" {
		return fc.ActrunnerDockerHost
	}
	bin, err := bashyResolver.Path(ctx)
	if err != nil {
		slog.Warn("act_runner sandbox: cannot resolve bashy to find the podman socket; set actrunner_docker_host", "err", err)
		return ""
	}
	out, err := exec.CommandContext(ctx, bin, "podman", "machine", "inspect",
		"--format", "{{.ConnectionInfo.PodmanSocket.Path}}").Output()
	if err != nil {
		slog.Warn("act_runner sandbox: `bashy podman machine inspect` failed; set actrunner_docker_host or start the podman machine", "err", err)
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return ""
	}
	if strings.Contains(p, "://") {
		return p
	}
	return "unix://" + p
}

// clusterAPIPort is the loopback port the STCP visitor binds for the
// apiserver. Mirrors the default the runtime uses.
func clusterAPIPort(cc *conf.ClusterConfig) int {
	if cc != nil && cc.K8sAPIPort != 0 {
		return cc.K8sAPIPort
	}
	return 6443
}

// peerVirtualAPIURL is the host-side endpoint for a peer plane. When the
// physical agent runtime is present it owns the authenticated STCP visitor in
// its container namespace and publishes it on a dedicated host-loopback port
// for virtual nodes. Virtual-only configurations retain the existing local
// visitor address until they gain a dedicated tunnel sidecar.
func peerVirtualAPIURL(cc *conf.ClusterConfig) string {
	if cc != nil && cc.JoinsPeerPlane() && cc.HasAgentRuntime() {
		return fmt.Sprintf("https://127.0.0.1:%d", runtime.DefaultPeerAPIBridgePort)
	}
	return cc.LocalAPIURL()
}

// isLoopbackURL reports whether a URL's host is loopback — i.e. the apiserver
// is reached on this machine rather than across the network.
func isLoopbackURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// hasHostedJoinToken reports whether this host has a tunnel token, meaning
// it can accept workers joining its hosted control plane. TunnelToken is the
// credential THIS host uses to run a plane and accept joins; it is distinct
// from JoinToken, which is what THIS host uses to join ANOTHER host's plane.
func hasHostedJoinToken(cc *conf.ClusterConfig) bool {
	return cc != nil && strings.TrimSpace(cc.TunnelToken) != ""
}
