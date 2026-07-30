package main

import (
	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/recipebuilder"
)

func clusterRecipeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recipe",
		Short: "Author portable DKS image recipes",
	}
	cmd.AddCommand(clusterRecipePackCmd())
	return cmd
}

func clusterRecipePackCmd() *cobra.Command {
	var spec recipebuilder.InlineRecipeSpec
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Pack an explicit source whitelist into a digest-verified inline recipe",
		Long: `Create a portable ImageRecipe on stdout from explicitly selected
files below a context directory. The generated recipe carries a deterministic,
bounded tar.gz context and its SHA-256; it contains no image blob.

The command only authors the recipe. Publish its output with an operator-issued
recipes:write credential using PUT /api/v1/recipes/<name>.`,
		Example: `  outpost cluster recipe pack \
    --name example --tag v1 --local-ref localhost/cluster/example \
    --context ./project --dockerfile build/Dockerfile \
    --include go.mod --include go.sum --include build --include cmd/example \
    > example-recipe.yaml`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return recipebuilder.PackInlineRecipe(cmd.OutOrStdout(), spec)
		},
	}
	cmd.Flags().StringVar(&spec.Name, "name", "", "recipe name")
	cmd.Flags().StringVar(&spec.Tag, "tag", "latest", "image tag")
	cmd.Flags().StringVar(&spec.LocalRef, "local-ref", "", "node-local image reference without tag")
	cmd.Flags().StringVar(&spec.ContextDir, "context", ".", "build context root")
	cmd.Flags().StringVar(&spec.Dockerfile, "dockerfile", "", "Dockerfile path relative to the context root")
	cmd.Flags().StringArrayVar(&spec.Includes, "include", nil, "relative file or directory to include (repeatable)")
	cmd.Flags().StringArrayVar(&spec.BaseImages, "base-image", nil, "declared base image (repeatable)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("local-ref")
	_ = cmd.MarkFlagRequired("dockerfile")
	_ = cmd.MarkFlagRequired("include")
	return cmd
}
