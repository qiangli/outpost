package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// outpost mcp {endpoint, rotate-token} — operator/scripting access to
// the MCP credentials surface. `endpoint` reads the persisted bearer
// directly off the FileConfig (mode 0600 — same OS user); useful for
// installer scripts that want to write a .mcp.json snippet without
// going through the admin UI. `rotate-token` calls the MCP server's
// rotation tool (daemon must be running) so the in-memory token swap
// stays consistent.
func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Show MCP endpoint / bearer token, or rotate it",
	}
	cmd.AddCommand(mcpEndpointCmd(), mcpRotateTokenCmd())
	return cmd
}

func mcpEndpointCmd() *cobra.Command {
	var snippet bool
	cmd := &cobra.Command{
		Use:   "endpoint",
		Short: "Print the MCP endpoint URL and bearer token (reads agent.json directly)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, err := conf.DefaultConfigPath()
			if err != nil {
				return err
			}
			fc, err := conf.LoadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("read %s: %w", cfgPath, err)
			}
			if fc == nil || fc.MCPBearerToken == "" {
				return fmt.Errorf("no MCP bearer token in %s — run `outpost start` once to generate one", cfgPath)
			}
			url := localMCPEndpointURL() + "/"
			if !snippet {
				fmt.Printf("endpoint  %s\n", url)
				fmt.Printf("bearer    %s\n", fc.MCPBearerToken)
				return nil
			}
			fmt.Print(renderMCPJSON(url, fc.MCPBearerToken))
			return nil
		},
	}
	cmd.Flags().BoolVar(&snippet, "snippet", false, "Print a .mcp.json snippet you can paste into an agent tool's config")
	return cmd
}

func mcpRotateTokenCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rotate-token",
		Short: "Rotate the MCP bearer token (the old token stops working immediately)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Println("This rotates the MCP bearer in agent.json AND swaps the daemon's in-memory copy.")
				fmt.Println("Any agent tool with the OLD token in its .mcp.json will fail until you update it.")
				fmt.Println("Re-run with --yes to confirm.")
				return nil
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				NewBearerToken string `json:"new_bearer_token"`
			}
			if err := session.callTool(cmd.Context(), "outpost_rotate_mcp_token", map[string]any{}, &out); err != nil {
				return err
			}
			fmt.Println(out.NewBearerToken)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

// renderMCPJSON returns the Claude-Code-shape .mcp.json snippet so an
// installer script can `outpost mcp endpoint --snippet > .mcp.json`
// in one shot.
func renderMCPJSON(url, token string) string {
	return `{
  "mcpServers": {
    "outpost": {
      "type": "http",
      "url": "` + url + `",
      "headers": {
        "Authorization": "Bearer ` + token + `"
      }
    }
  }
}
`
}
