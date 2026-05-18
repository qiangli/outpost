// Command matrix-agent runs on a home host: it dials the cloud over frp
// and surfaces local apps (ycode, shell, desktop) through the tunnel.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

func main() {
	root := &cobra.Command{
		Use:   "matrix-agent",
		Short: "Matrix home-host agent — one node in your matrix",
	}
	root.AddCommand(startCmd(), registerCmd())
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
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the local agent, dial the cloud frp server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := conf.Load()
			if err != nil {
				return err
			}

			// Layer in saved file config (if any) before flag overrides.
			if path, _ := conf.DefaultConfigPath(); path != "" {
				if fc, _ := conf.LoadFile(path); fc != nil {
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
				}
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
			if cfg.AgentName == "" {
				return errors.New("AgentName is empty: run `matrix-agent register` first or set $AGENT_NAME")
			}

			apps, err := buildAppRegistry(cfg.Apps)
			if err != nil {
				return err
			}

			admins := agent.NewAdminSet(cfg.AdminUsers)
			engine := gin.Default()
			agent.RegisterRoutes(engine.Group("/"), agent.Deps{
				AgentName: cfg.AgentName,
				Apps:      apps,
				Admins:    admins,
				AuthURL:   cfg.AuthURL,
				VNCAddr:   vncAddrFlag,
			})

			// Bind the local listener first so we know its port before
			// telling frps how to reach us.
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

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			g, gctx := errgroup.WithContext(ctx)
			g.Go(func() error {
				slog.Info("matrix-agent: local http listening", "addr", ln.Addr().String(), "name", cfg.AgentName, "apps", apps.Names())
				if err := localSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			})
			g.Go(func() error {
				slog.Info("matrix-agent: dialing frp", "server", cfg.ServerAddr, "port", cfg.ServerPort, "remotePort", cfg.RemotePort)
				return tunnel.Run(gctx)
			})
			g.Go(func() error {
				<-gctx.Done()
				slog.Info("matrix-agent: shutting down")
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = localSrv.Shutdown(shutdownCtx)
				tunnel.Close()
				return nil
			})
			return g.Wait()
		},
	}
	cmd.Flags().StringVar(&addrFlag, "addr", "", "Local loopback HTTP listen address (overrides $AGENT_ADDR)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "Agent name displayed in the portal (overrides $AGENT_NAME)")
	cmd.Flags().StringVar(&serverAddrFlag, "server", "", "frps host (overrides $FRP_SERVER_ADDR)")
	cmd.Flags().IntVar(&serverPortFlag, "server-port", 0, "frps port (overrides $FRP_SERVER_PORT)")
	cmd.Flags().StringVar(&vncAddrFlag, "vnc-addr", "127.0.0.1:5900", "VNC server to expose for the desktop tab")
	return cmd
}

// buildAppRegistry parses MATRIX_APPS ("name1=url1,name2=url2") and seeds
// `ycode → http://127.0.0.1:8765` when no apps are configured at all.
func buildAppRegistry(specs string) (*agent.AppRegistry, error) {
	reg := agent.NewAppRegistry()
	specs = strings.TrimSpace(specs)
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
		serverURL string
		code      string
		name      string
		out       string
		authURL   string
		title     string
	)
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Exchange a one-time code from the portal for a persistent agent config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if serverURL == "" || code == "" || name == "" {
				return errors.New("--server, --code, and --name are all required")
			}
			title = strings.TrimSpace(title)
			trimmedAuthURL := strings.TrimSpace(authURL)
			// A custom auth URL means the app users have no OS identity,
			// so the OS-derived subtitle would be misleading. Require a
			// human title in that case.
			if trimmedAuthURL != "" && title == "" {
				return errors.New("--title is required when --auth-url is set (no OS user to derive a subtitle from)")
			}

			// Report OS-side identity to the cloud so the portal can render
			// a disambiguating subtitle and the elevate form can prefill
			// the username. Best-effort: empty strings are fine.
			osUser, _ := hostauth.CurrentUser()
			osDisplay := hostauth.CurrentDisplayName()
			osHostname, _ := os.Hostname()

			payload := map[string]any{
				"code":            code,
				"name":            name,
				"title":           title,
				"os_user":         osUser,
				"os_display_name": osDisplay,
				"os_hostname":     osHostname,
				"has_auth_url":    trimmedAuthURL != "",
			}
			body, _ := json.Marshal(payload)
			req, err := http.NewRequestWithContext(cmd.Context(), "POST",
				strings.TrimRight(serverURL, "/")+"/api/register/exchange",
				bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("exchange: %w", err)
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("exchange failed (%d): %s", resp.StatusCode, bytes.TrimSpace(respBody))
			}

			var ex struct {
				AgentName  string `json:"agent_name"`
				ServerAddr string `json:"server_addr"`
				ServerPort int    `json:"server_port"`
				Protocol   string `json:"protocol"`
				Token      string `json:"token"`
				RemotePort int    `json:"remote_port"`
			}
			if err := json.Unmarshal(respBody, &ex); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			fc := &conf.FileConfig{
				AgentName:  ex.AgentName,
				ServerAddr: ex.ServerAddr,
				ServerPort: ex.ServerPort,
				Protocol:   ex.Protocol,
				Token:      ex.Token,
				RemotePort: ex.RemotePort,
				AuthURL:    trimmedAuthURL,
			}
			path := out
			if path == "" {
				p, err := conf.DefaultConfigPath()
				if err != nil {
					return err
				}
				path = p
			}
			if err := conf.SaveFile(path, fc); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Printf("Registered as %q (remote port %d). Config saved to %s\n", fc.AgentName, fc.RemotePort, path)
			fmt.Printf("Start the agent with: matrix-agent start\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "Cloud portal URL, e.g. https://cloud.example.com")
	cmd.Flags().StringVar(&code, "code", "", "One-time registration code from the portal")
	cmd.Flags().StringVar(&name, "name", "", "Host name to display in the portal")
	cmd.Flags().StringVar(&out, "out", "", "Output config path (default ~/.config/matrix/agent.json)")
	cmd.Flags().StringVar(&authURL, "auth-url", "",
		"Optional application-level auth endpoint. When set, the agent forwards {user,password} to it and trusts the returned role; the host OS is no longer consulted.")
	cmd.Flags().StringVar(&title, "title", "",
		"Human-readable subtitle shown in the portal (e.g. \"Family streaming box\"). Required when --auth-url is set; optional otherwise (falls back to the OS user / hostname).")
	return cmd
}
