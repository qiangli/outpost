package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/peerimage"
)

// peerImageCmd is the CLI surface of peer image distribution ("recipes, not
// blobs" — docs/peer-dks-image-distribution.md). Every subcommand is a thin
// MCP call into the ONE admincore method for its verb; the MCP tools
// (mcpapi/tools_peerimage.go) and the file key (conf.PeerImageConfig) reach
// the same methods, which is what the parity tests prove.
//
// The tool names come from admincore.PeerImageToolNames rather than string
// literals so a rename cannot desynchronize the two surfaces.
//
// Side-effect classes: all four verbs are Live (no restart). The boot-time
// half — whether the recipe index is served at all — is the peer_image file
// key, which is Boot-only (see docs/settings.md).

func peerImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer-image",
		Short: "Peer image distribution (recipes, not blobs): publish / mesh-resolve / ensure / report",
	}
	cmd.AddCommand(
		peerImagePublishCmd(),
		peerImageListCmd(),
		peerImageMeshResolveCmd(),
		peerImageEnsureCmd(),
		peerImageReportCmd(),
	)
	return cmd
}

// peerImageTool resolves a verb's MCP tool name from the single shared table.
func peerImageTool(verb admincore.PeerImageVerb) string {
	return admincore.PeerImageToolNames[verb]
}

func peerImagePublishCmd() *cobra.Command {
	var (
		name    string
		file    string
		body    string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish a build recipe so peers can fetch and build it themselves (no image blob moves)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			text, err := peerImageRecipeBody(file, body, cmd.InOrStdin())
			if err != nil {
				return err
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Publication peerimage.Publication `json:"publication"`
			}
			err = session.callTool(cmd.Context(), peerImageTool(admincore.PeerImageVerbPublish),
				map[string]any{"name": name, "body": text}, &out)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(out.Publication)
			}
			fmt.Printf("Published recipe %q as %s (digest %s)\n",
				out.Publication.Name, out.Publication.Ref, out.Publication.Digest)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Recipe name (optional when the document names itself; must match when both are given)")
	cmd.Flags().StringVar(&file, "file", "", "Path to the recipe YAML (default: read stdin when --body is absent)")
	cmd.Flags().StringVar(&body, "body", "", "Inline recipe YAML (overrides --file and stdin)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the publication as JSON")
	return cmd
}

// peerImageRecipeBody resolves the recipe document from --body, --file, or
// stdin, in that precedence. A recipe is required — publishing nothing is an
// error, never a silent no-op.
func peerImageRecipeBody(file, body string, stdin io.Reader) (string, error) {
	if strings.TrimSpace(body) != "" {
		return body, nil
	}
	if strings.TrimSpace(file) != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read recipe file: %w", err)
		}
		return string(b), nil
	}
	if stdin == nil {
		return "", errors.New("a recipe is required (--body, --file, or stdin)")
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read recipe from stdin: %w", err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("a recipe is required (--body, --file, or stdin)")
	}
	return string(b), nil
}

func peerImageListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the recipes this node currently publishes to peers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Recipes []peerimage.Publication `json:"recipes"`
			}
			if err := session.callTool(cmd.Context(), "outpost_list_image_recipes", map[string]any{}, &out); err != nil {
				return err
			}
			if jsonOut {
				return printJSON(out.Recipes)
			}
			if len(out.Recipes) == 0 {
				fmt.Println("No recipes published.")
				return nil
			}
			for _, p := range out.Recipes {
				fmt.Printf("%s\t%s\t%s\n", p.Name, p.Ref, p.Digest)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the list as JSON")
	return cmd
}

func peerImageMeshResolveCmd() *cobra.Command {
	var (
		service string
		minimum int
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "mesh-resolve",
		Short: "Find the DISTINCT peers exposing the recipe service over the mesh",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Result peerimage.ResolveResult `json:"result"`
			}
			err = session.callTool(cmd.Context(), peerImageTool(admincore.PeerImageVerbMeshResolve),
				map[string]any{"service": service, "minimum": minimum}, &out)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(out.Result)
			}
			fmt.Printf("service=%s distinct=%d\n", out.Result.Service, out.Result.Distinct)
			for _, p := range out.Result.Peers {
				fmt.Printf("%s\t%s\n", p.Host, p.PeerID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "Mesh service name to resolve (default: the configured peer_image.service)")
	cmd.Flags().IntVar(&minimum, "minimum", 1, "Minimum number of DISTINCT peers required; falling short is an error")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the result as JSON")
	return cmd
}

func peerImageEnsureCmd() *cobra.Command {
	var (
		name    string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Make a published recipe's image resident on THIS node, confirmed by containerd content digest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Result peerimage.EnsureResult `json:"result"`
			}
			err = session.callTool(cmd.Context(), peerImageTool(admincore.PeerImageVerbEnsure),
				map[string]any{"name": name}, &out)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(out.Result)
			}
			fmt.Printf("node=%s ref=%s state=%s built=%t digest=%s\n",
				out.Result.Node, out.Result.Ref, out.Result.State, out.Result.Built, out.Result.ContentDigest)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Name of a recipe in the local store (publish or fetch it first)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the result as JSON")
	return cmd
}

func peerImageReportCmd() *cobra.Command {
	var (
		node         string
		ref          string
		recipeDigest string
		nonce        string
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Answer an inspector's identity-bound challenge with this node's evidence",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var out struct {
				Report peerimage.Report `json:"report"`
			}
			err = session.callTool(cmd.Context(), peerImageTool(admincore.PeerImageVerbReport),
				map[string]any{
					"node": node, "ref": ref, "recipe_digest": recipeDigest, "nonce": nonce,
				}, &out)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(out.Report)
			}
			fmt.Printf("node=%s ref=%s state=%s digest=%s provenance=%s\n",
				out.Report.Node, out.Report.Ref, out.Report.State, out.Report.ContentDigest, out.Report.ProvenanceDigest)
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Node the challenge addresses (answered only when it names THIS node or is empty)")
	cmd.Flags().StringVar(&ref, "ref", "", "Image reference the evidence is about")
	cmd.Flags().StringVar(&recipeDigest, "recipe-digest", "", "The recipe's cross-node identity, sha256:<64 hex>")
	cmd.Flags().StringVar(&nonce, "nonce", "", "The per-(node,ref,recipe) challenge nonce (required — unbound evidence cannot be attributed)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the report as JSON")
	return cmd
}

// printJSON renders v as indented JSON on stdout. Secrets never flow through
// these results by design (digests, names, refs), and this host's PTY capture
// is unredacted — no path here prints a credential.
func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// peerImageRunForTest drives a peer-image subcommand's RunE against an
// already-dialed target. It exists so the parity test can point the CLI at an
// in-process MCP server and assert it lands on the same admincore method the
// MCP tool does. It is not used in production paths.
func peerImageRunForTest(ctx context.Context, cmd *cobra.Command, args []string) error {
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}
