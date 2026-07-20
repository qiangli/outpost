package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// outpost token print exposes the persisted cloudbox access_token to
// same-OS-user local tools. It is intentionally file-only: no daemon,
// socket, or network endpoint is involved.
func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "token",
		Short:        "Print local persisted tokens",
		SilenceUsage: true,
	}
	cmd.AddCommand(tokenPrintCmd())
	return cmd
}

func tokenPrintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "print",
		Short: "Print the cloudbox access_token from agent.json",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, err := conf.DefaultConfigPath()
			if err != nil {
				return err
			}
			fc, err := conf.LoadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("read %s: %w", cfgPath, err)
			}
			if fc == nil || strings.TrimSpace(fc.AccessToken) == "" {
				return fmt.Errorf("no cloudbox access_token in %s — pair with `outpost register` first", cfgPath)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), fc.AccessToken)
			return err
		},
	}
}
