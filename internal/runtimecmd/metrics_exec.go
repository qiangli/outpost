package runtimecmd

import "github.com/spf13/cobra"

func metricsExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "metrics-exec [metrics-server flags]",
		Short:              "Run the colocated metrics server without root privileges",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE:               func(cmd *cobra.Command, args []string) error { return execMetrics(args) },
	}
}
