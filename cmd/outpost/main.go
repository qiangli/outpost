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
	"github.com/qiangli/outpost/internal/agent/adminui"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/ollama"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
	"github.com/qiangli/outpost/internal/agent/portal"
	"github.com/qiangli/outpost/internal/agent/vkpodman"
)

// defaultPortal is the public ai.dhnt.io address used when the user
// doesn't override it via --server or the interactive prompt.
const defaultPortal = "https://ai.dhnt.io"

func main() {
	root := &cobra.Command{
		Use:   "outpost",
		Short: "Pair a home host with the portal and tunnel local apps to it",
	}
	root.AddCommand(startCmd(), registerCmd(), stopCmd(), sshProxyCmd(), sshConfigCmd(), connectCmd(), outboundCmd(), jobsCmd(), fgCmd(), bgCmd(), killCmd(), runCmd(), clusterCmd(), poolCmd())
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

			// Layer fc into cfg (env values stay primary unless empty).
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

			adminAddr := adminAddrFlag
			if adminAddr == "" {
				adminAddr = os.Getenv("OUTPOST_ADMIN_ADDR")
			}
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
			llmPoolStatus := func() adminui.LLMPoolStatusView {
				if ollamaSvc == nil {
					return adminui.LLMPoolStatusView{}
				}
				st := ollamaSvc.Status()
				return adminui.LLMPoolStatusView{
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

			adminSrv, err := adminui.New(adminui.Deps{
				ConfigPath:    cfgPath,
				ListenAddr:    adminAddr,
				Auth:          hostauth.DefaultAuthenticator(),
				Apps:          apps,
				Restart:       restartFn,
				SessionKey:    sessionKey,
				Outbound:      outbound,
				LLMPoolStatus: llmPoolStatus,
			})
			if err != nil {
				return fmt.Errorf("admin ui: %w", err)
			}

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
				VNCAddr:               vncAddrFlag,
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

			tunnel, err := agent.NewTunnel(agent.TunnelConfig{
				ServerAddr: cfg.ServerAddr,
				ServerPort: cfg.ServerPort,
				Protocol:   cfg.Protocol,
				Token:      cfg.Token,
				User:       cfg.AgentName,
			}, []agent.TCPProxy{{
				Name:       cfg.AgentName + "-http",
				LocalIP:    "127.0.0.1",
				LocalPort:  localPort,
				RemotePort: cfg.RemotePort,
			}})
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
				if err := startClusterRunner(gctx, g, fc, cfgPath); err != nil {
					slog.Warn("cluster mode: disabled", "err", err)
				}
			}
			g.Go(func() error {
				<-gctx.Done()
				slog.Info("matrix-agent: shutting down")
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
	cmd.Flags().StringVar(&vncAddrFlag, "vnc-addr", "127.0.0.1:5900", "VNC server to expose for the desktop tab")
	cmd.Flags().StringVar(&adminAddrFlag, "admin-addr", "", "Admin UI listen address (overrides $OUTPOST_ADMIN_ADDR; default 127.0.0.1:17777). Use 0.0.0.0:17777 to expose to the LAN; login is then always required.")
	return cmd
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
func startClusterRunner(ctx context.Context, g *errgroup.Group, fc *conf.FileConfig, cfgPath string) error {
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
			NodeName:     nodeName,
			PodmanSocket: bt.Socket,
			Kube:         kubeCfg,
			Access:       access,
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
	for _, entry := range strings.Split(specs, ",") {
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
		Use:   "register",
		Short: "Pair this host with the portal",
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

// outpostLogPath returns ~/.cache/outpost/outpost.log (or the OS
// equivalent), creating parent directories as needed.
func outpostLogPath() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "outpost")
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
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "outpost")
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
			// Poll for graceful exit, up to 5 s.
			for i := 0; i < 50; i++ {
				if !processAlive(pid) {
					_ = os.Remove(p)
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
