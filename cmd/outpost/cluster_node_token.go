package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// `outpost cluster token` — the k3s node token of the plane hosted HERE.
//
// This is the last of the four values a worker needs, and the only one outpost
// does not mint itself: k3s writes it inside the control-plane container. Until
// this command existed the documented join procedure ended with a `podman exec
// … cat` typed by hand, which is not a product.
//
// The token goes to STDOUT and nothing else. Everything explanatory goes to
// stderr, so `outpost cluster token | ssh worker outpost cluster join … --token-stdin`
// pipes the exact bytes and a human running it still sees what they got.
func clusterNodeTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print the k3s node token of the control plane hosted on this machine",
		Long: `Print the k3s node token workers pass to ` + "`outpost cluster join --node-token`" + `.

It is one of the three credentials a worker needs; the other two come from
` + "`outpost cluster control-plane token`" + `.

This is a CREDENTIAL. It is written to stdout only — never to the daemon log —
so it can be piped without a human ever seeing it:

    outpost cluster token | ssh worker outpost cluster join 10.0.0.5 --token-stdin

Only the machine HOSTING the plane has it. On a worker this fails with a
pointer to the right machine rather than printing an empty line.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				NodeToken string `json:"node_token"`
				Endpoint  string `json:"endpoint"`
			}
			if err := session.callTool(cmd.Context(), "outpost_cluster_node_token", map[string]any{}, &out); err != nil {
				return err
			}
			if out.NodeToken == "" {
				return fmt.Errorf("control plane returned no node token — is it still starting? (`outpost cluster control-plane`)")
			}
			fmt.Fprintln(os.Stdout, out.NodeToken)
			if out.Endpoint != "" {
				fmt.Fprintf(os.Stderr, "(workers pair this with endpoint %s)\n", out.Endpoint)
			}
			return nil
		},
	}
	return cmd
}
