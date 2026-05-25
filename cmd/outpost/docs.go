package main

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// embeddedDocs ships the markdown references under docs/ inside the
// binary so `outpost docs <topic>` works on a host that doesn't have
// the source tree available. Topics are addressed by filename without
// the .md extension (e.g. `outpost docs settings` renders
// docs/settings.md).
//
// Only docs intended for operator reference are listed in
// docsManifest; that keeps the help-topic surface focused (we don't
// surface design-notes or matrix-shell-deferred-bugs to end users
// browsing the CLI).
//
//go:embed all:embedded_docs
var embeddedDocs embed.FS

// docsManifest is the curated list of topic IDs the `outpost docs`
// command surfaces, in display order, with a one-line description.
// Adding a new help topic = drop a markdown file into embedded_docs/
// and append a row here.
var docsManifest = []struct {
	Topic       string
	Title       string
	Description string
}{
	{"settings", "Settings reference", "Every persistable setting: file key, CLI flag, UI location, MCP tool, side-effects."},
	{"mcp", "MCP server guide", "Setup, tool catalog, .mcp.json snippet, token rotation, security posture."},
}

func docsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs [topic]",
		Short: "Browse embedded reference documentation",
		Long: `Render markdown reference docs that ship inside the outpost binary.
Run without a topic to list what's available, or pass a topic name
to print it (suitable for piping through less / glow).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return listDocs(cmd)
			}
			return printDoc(cmd, args[0])
		},
	}
	cmd.AddCommand(docsListCmd())
	return cmd
}

func docsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available topics",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listDocs(cmd)
		},
	}
}

func listDocs(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Available topics:")
	fmt.Fprintln(out)
	width := 0
	for _, d := range docsManifest {
		if len(d.Topic) > width {
			width = len(d.Topic)
		}
	}
	for _, d := range docsManifest {
		fmt.Fprintf(out, "  %-*s   %s\n", width, d.Topic, d.Title)
		fmt.Fprintf(out, "  %s   %s\n", strings.Repeat(" ", width), d.Description)
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "View a topic:  outpost docs <topic>\n")
	return nil
}

func printDoc(cmd *cobra.Command, topic string) error {
	// Resolve the topic against the manifest first so the operator
	// gets a clear "unknown topic" error when they typo, rather than
	// a generic file-not-found.
	known := false
	for _, d := range docsManifest {
		if d.Topic == topic {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("unknown topic %q — run `outpost docs` to list available topics", topic)
	}
	b, err := fs.ReadFile(embeddedDocs, "embedded_docs/"+topic+".md")
	if err != nil {
		return fmt.Errorf("read embedded doc %q: %w", topic, err)
	}
	_, err = cmd.OutOrStdout().Write(b)
	return err
}

// validateDocsManifest is called from a test to ensure every topic in
// docsManifest has a matching markdown file embedded. Kept here so the
// list and the embedded-fs slice stay in sync across refactors.
func validateDocsManifest() error {
	entries, err := fs.ReadDir(embeddedDocs, "embedded_docs")
	if err != nil {
		return fmt.Errorf("read embedded_docs dir: %w", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			present[strings.TrimSuffix(e.Name(), ".md")] = true
		}
	}
	missing := []string{}
	for _, d := range docsManifest {
		if !present[d.Topic] {
			missing = append(missing, d.Topic)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("manifest references topics with no embedded file: %v", missing)
	}
	return nil
}
