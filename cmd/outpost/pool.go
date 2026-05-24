// `outpost pool status` is the operator's read-only view of this
// outpost's LLM-pool participation. Inspects the persisted FileConfig
// + probes the locally-configured Ollama daemon directly — does not
// talk to the running outpost process (no admin-UI login dance, no
// session cookie). For live watcher state (last push time, in-flight
// counter), open the admin UI; this CLI is for "is the pool wired
// correctly?" sanity checks and scripting.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func poolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Inspect this outpost's LLM-pool participation",
	}
	cmd.AddCommand(poolStatusCmd())
	return cmd
}

func poolStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show pool wiring + models the local Ollama would publish",
		Long: `Reads agent.json and probes the local Ollama daemon (default
http://127.0.0.1:11434, or $OLLAMA_HOST). Prints whether the pool is
enabled, the cloudbox URL the watcher pushes to, and the model
inventory currently visible to the watcher.

This is a sanity check — it does NOT query the running outpost for
live watcher state. For that, open the admin UI (its /api/config
endpoint includes the same diagnostic block).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := conf.DefaultConfigPath()
			var fc *conf.FileConfig
			if cfgPath != "" {
				fc, _ = conf.LoadFile(cfgPath)
			}
			if fc == nil {
				fc = &conf.FileConfig{}
			}

			st := poolReport(cmd.Context(), fc)

			if jsonOut {
				b, _ := json.MarshalIndent(st, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			return printPoolStatus(cmd.OutOrStdout(), st)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the status as JSON (for scripting).")
	return cmd
}

// poolStatusReport is the structured view printPoolStatus + --json both
// render. Kept here (not in the ollama package) because it carries
// CLI-only fields (effective config path, detected URL) that the
// in-process watcher doesn't need to know about.
type poolStatusReport struct {
	ConfigPath     string             `json:"config_path"`
	Paired         bool               `json:"paired"`
	AgentName      string             `json:"agent_name,omitempty"`
	OllamaEnabled  bool               `json:"ollama_enabled"`
	PoolEnabled    bool               `json:"pool_enabled"`
	OllamaURL      string             `json:"ollama_url"`
	OllamaReached  bool               `json:"ollama_reached"`
	CloudboxURL    string             `json:"cloudbox_url,omitempty"`
	HasAccessToken bool               `json:"has_access_token"`
	ProbedAt       time.Time          `json:"probed_at"`
	Models         []poolStatusModel  `json:"models,omitempty"`
	ProbeError     string             `json:"probe_error,omitempty"`
	Notes          []string           `json:"notes,omitempty"`
}

type poolStatusModel struct {
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
}

func poolReport(ctx context.Context, fc *conf.FileConfig) poolStatusReport {
	cfgPath, _ := conf.DefaultConfigPath()
	st := poolStatusReport{
		ConfigPath:     cfgPath,
		Paired:         fc.AgentName != "",
		AgentName:      fc.AgentName,
		OllamaEnabled:  fc.OllamaOn(),
		PoolEnabled:    fc.OllamaPoolOn(),
		HasAccessToken: fc.AccessToken != "",
		CloudboxURL:    cloudboxHTTPBase(fc),
		OllamaURL:      "http://127.0.0.1:11434",
		ProbedAt:       time.Now().UTC(),
	}
	bt := agent.DetectOllama()
	if bt.URL != "" {
		st.OllamaURL = bt.URL
	}
	st.OllamaReached = bt.Available

	if !st.OllamaEnabled {
		st.Notes = append(st.Notes, "Ollama built-in is OFF in agent.json — toggle it on in the admin UI.")
	}
	if st.OllamaEnabled && !st.OllamaReached {
		st.Notes = append(st.Notes, "Ollama daemon not reachable at "+st.OllamaURL+" — set $OLLAMA_HOST or start `ollama serve`.")
	}
	if st.OllamaEnabled && !st.PoolEnabled {
		st.Notes = append(st.Notes, "Pool participation is OFF (the local Ollama is private to this host).")
	}
	if st.PoolEnabled && !st.HasAccessToken {
		st.Notes = append(st.Notes, "No access_token in agent.json — outpost is unpaired; the watcher would no-op.")
	}
	if st.PoolEnabled && st.CloudboxURL == "" {
		st.Notes = append(st.Notes, "Cloudbox URL is empty — re-pair via `outpost register`.")
	}

	if st.OllamaReached {
		models, err := fetchOllamaTags(ctx, st.OllamaURL)
		if err != nil {
			st.ProbeError = err.Error()
		} else {
			st.Models = models
		}
	}
	return st
}

// fetchOllamaTags GETs /api/tags directly so the CLI sees exactly what
// the watcher would publish. Tiny ad-hoc decoder rather than importing
// the ollama package's internal types — the CLI surface should be
// stable regardless of internal refactors.
func fetchOllamaTags(ctx context.Context, base string) ([]poolStatusModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			Size  int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]poolStatusModel, 0, len(payload.Models))
	for _, m := range payload.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		if name == "" {
			continue
		}
		out = append(out, poolStatusModel{Name: name, Size: m.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func printPoolStatus(w io.Writer, st poolStatusReport) error {
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(w, "Config:        %s\n", st.ConfigPath)
	fmt.Fprintf(w, "Paired:        %s", yn(st.Paired))
	if st.AgentName != "" {
		fmt.Fprintf(w, "  (agent=%s)", st.AgentName)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Ollama on:     %s\n", yn(st.OllamaEnabled))
	fmt.Fprintf(w, "Pool on:       %s\n", yn(st.PoolEnabled))
	fmt.Fprintf(w, "Access token:  %s\n", yn(st.HasAccessToken))
	if st.CloudboxURL != "" {
		fmt.Fprintf(w, "Cloudbox URL:  %s\n", st.CloudboxURL)
	}
	fmt.Fprintf(w, "Ollama URL:    %s  (reachable=%s)\n", st.OllamaURL, yn(st.OllamaReached))

	if st.ProbeError != "" {
		fmt.Fprintf(w, "\nOllama probe error: %s\n", st.ProbeError)
	}
	if len(st.Models) > 0 {
		fmt.Fprintf(w, "\nModels currently loaded (%d):\n", len(st.Models))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tSIZE")
		for _, m := range st.Models {
			fmt.Fprintf(tw, "  %s\t%s\n", m.Name, humanBytes(m.Size))
		}
		_ = tw.Flush()
	} else if st.OllamaReached {
		fmt.Fprintln(w, "\nOllama responded but has no models loaded.")
	}

	if len(st.Notes) > 0 {
		fmt.Fprintln(w, "\nNotes:")
		for _, n := range st.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	if !st.OllamaEnabled || !st.PoolEnabled {
		_, _ = fmt.Fprint(os.Stderr) // ensure stderr isn't lazily-unflushed
	}
	return nil
}

// humanBytes formats a byte count as KB/MB/GB. Mirrors what `ollama
// list` shows, so the CLI output reads naturally next to it.
func humanBytes(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		f /= k
		if f < k {
			return fmt.Sprintf("%.1f%s", f, u)
		}
	}
	return fmt.Sprintf("%.1fPB", f/k)
}
