package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent"
)

// versionCmd emits the running CLI binary's build provenance. Two forms:
//
//   - default: one human line, "<short-commit> (<go-version>) built <vcs-time>"
//   - --json:  the full BuildInfo struct, the same shape GET /version returns
//
// The JSON form is the stable verification probe used by `outpost upgrade`
// to confirm a candidate binary is a real outpost build before it gets
// renamed over the live one. Don't break the JSON shape without bumping
// the verification path.
func versionCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print outpost build provenance (commit, dirty flag, Go version)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b := agent.ReadBuildInfo()
			if jsonOut {
				out, _ := json.MarshalIndent(b, "", "  ")
				fmt.Println(string(out))
				return nil
			}
			line := b.Short()
			if b.GoVersion != "" {
				line += " (" + b.GoVersion + ")"
			}
			if b.VCSTime != "" {
				line += " built " + b.VCSTime
			}
			fmt.Println(line)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit BuildInfo as JSON (stable probe shape)")
	return cmd
}
