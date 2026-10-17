// Package runtimecmd provides the pure-Go utilities used by the runtime image.
package runtimecmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/policy/security"
	frplog "github.com/fatedier/frp/pkg/util/log"
	"github.com/fatedier/frp/server"
	"github.com/spf13/cobra"
)

// Register adds the runtime utilities to outpost's root command.
func Register(root *cobra.Command) {
	root.AddCommand(frpCmd(false), frpCmd(true), certgenCmd(), tlsproxyCmd(), netaddrCmd(), metricsExecCmd(), etcdPeerCmd())
}

func signalContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
}

func frpCmd(isServer bool) *cobra.Command {
	name := "frpc"
	if isServer {
		name = "frps"
	}
	var path string
	cmd := &cobra.Command{Use: name + " -c file", Short: "Run the embedded FRP " + name, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext(cmd)
			defer cancel()
			return runFRP(ctx, path, isServer)
		}}
	cmd.Flags().StringVarP(&path, "config", "c", "", "FRP configuration file")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func runFRP(ctx context.Context, path string, isServer bool) error {
	unsafe := security.NewUnsafeFeatures(nil)
	if isServer {
		cfg, _, err := config.LoadServerConfig(path, true)
		if err != nil {
			return err
		}
		if _, err = validation.NewConfigValidator(unsafe).ValidateServerConfig(cfg); err != nil {
			return err
		}
		frplog.InitLogger(cfg.Log.To, cfg.Log.Level, int(cfg.Log.MaxDays), cfg.Log.DisablePrintColor)
		svc, err := server.NewService(cfg)
		if err != nil {
			return err
		}
		defer svc.Close()
		done := make(chan struct{})
		go func() { svc.Run(ctx); close(done) }()
		// FRP's multiplexed accept loop need not return after Close. Do not
		// wait for it on termination; Close releases the listening sockets.
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			return nil
		}
	}
	cfg, proxies, visitors, _, err := config.LoadClientConfig(path, true)
	if err != nil {
		return err
	}
	if cfg.Store.IsEnabled() || len(cfg.FeatureGates) != 0 {
		return fmt.Errorf("runtime frpc does not support store or feature gates")
	}
	if _, err = validation.ValidateAllClientConfig(cfg, proxies, visitors, unsafe); err != nil {
		return err
	}
	src := source.NewConfigSource()
	if err := src.ReplaceAll(proxies, visitors); err != nil {
		return err
	}
	frplog.InitLogger(cfg.Log.To, cfg.Log.Level, int(cfg.Log.MaxDays), cfg.Log.DisablePrintColor)
	svc, err := client.NewService(client.ServiceOptions{Common: cfg, ConfigSourceAggregator: source.NewAggregator(src), UnsafeFeatures: unsafe, ConfigFilePath: path})
	if err != nil {
		return err
	}
	defer svc.Close()
	return svc.Run(ctx)
}
