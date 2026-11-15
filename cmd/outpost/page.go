package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// page controls an already open admin view. Configuration writes still use
// admincore through the existing config API; these commands add live-page
// discovery and named handler invocation, never selectors or script execution.
func pageCmd() *cobra.Command {
	var cookieFile string
	cmd := &cobra.Command{Use: "page", Short: "Inspect and control an open Outpost admin page", SilenceUsage: true}
	cmd.PersistentFlags().Bool("json", true, "emit machine-readable JSON")
	cmd.PersistentFlags().StringVar(&cookieFile, "cookie-file", "", "file containing an admin session cookie (for an explicitly selected remote admin listener)")
	client := func() (*adminClient, error) {
		c, err := newAdminClient(false)
		if err != nil {
			return nil, err
		}
		c.strictStatus = true
		c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if cookieFile != "" {
			addr := rootDialOpts.Host
			if addr == "" && rootDialOpts.Remote != "" {
				entry, err := loadRemoteEntry(rootDialOpts.Remote)
				if err != nil {
					return nil, err
				}
				addr = entry.Addr
			}
			if addr == "" {
				addr = os.Getenv("OUTPOST_HOST")
			}
			if addr != "" {
				c.base = strings.TrimSuffix(normalizeMCPEndpoint(addr), "/mcp")
			}
			b, err := os.ReadFile(cookieFile)
			if err != nil {
				return nil, err
			}
			c.cookie = strings.TrimSpace(string(b))
			if c.cookie == "" {
				return nil, fmt.Errorf("empty admin cookie file")
			}
			return c, nil
		}
		selected := rootDialOpts.Host != "" || rootDialOpts.Remote != "" || rootDialOpts.Token != "" || os.Getenv("OUTPOST_HOST") != "" || os.Getenv("OUTPOST_MCP_TOKEN") != ""
		endpoint, token, _, resolveErr := resolveMCPTarget()
		if resolveErr == nil {
			base := strings.TrimSuffix(strings.TrimSuffix(endpoint, "/"), "/mcp")
			if base != c.base {
				c.cookie = ""
			}
			c.base, c.token = base, token
		} else if selected {
			return nil, resolveErr
		}
		return c, nil
	}
	print := func(c *cobra.Command, data []byte) error {
		_, err := fmt.Fprintln(c.OutOrStdout(), string(data))
		return err
	}
	cmd.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List connected pages and their mounted actions", RunE: func(c *cobra.Command, _ []string) error {
		a, err := client()
		if err != nil {
			return err
		}
		b, err := a.do(c.Context(), http.MethodGet, "/api/page/pages", nil)
		if err != nil {
			return err
		}
		return print(c, b)
	}})
	cmd.AddCommand(&cobra.Command{Use: "result ID", Args: cobra.ExactArgs(1), Short: "Read a command result without replaying it", RunE: func(c *cobra.Command, args []string) error {
		a, err := client()
		if err != nil {
			return err
		}
		b, err := a.do(c.Context(), http.MethodGet, "/api/page/result?id="+url.QueryEscape(args[0]), nil)
		if err != nil {
			return err
		}
		return print(c, b)
	}})
	invoke := func(c *cobra.Command, page, action string, input any) error {
		a, err := client()
		if err != nil {
			return err
		}
		b, err := a.do(c.Context(), http.MethodPost, "/api/page/invoke", map[string]any{"page_id": page, "action": action, "input": input})
		if err != nil {
			return fmt.Errorf("page invocation failed (outcome may be unknown if connection was lost): %w; not retried", err)
		}
		var result struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err = json.Unmarshal(b, &result); err != nil {
			return err
		}
		id := result.ID
		timer := time.NewTimer(35 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-c.Context().Done():
				return fmt.Errorf("command %s outcome unknown: %w; not retried", id, c.Context().Err())
			case <-timer.C:
				return fmt.Errorf("command %s outcome unknown; inspect with outpost page result %s; not retried", id, id)
			case <-tick.C:
				b, err = a.do(c.Context(), http.MethodGet, "/api/page/result?id="+url.QueryEscape(id), nil)
				if err != nil {
					return fmt.Errorf("command %s outcome unknown: %w; not retried", id, err)
				}
				if err = json.Unmarshal(b, &result); err != nil {
					return err
				}
				switch result.Status {
				case "completed":
					return print(c, b)
				case "failed", "expired":
					return fmt.Errorf("command %s %s: %s; not retried", id, result.Status, result.Error)
				}
			}
		}
	}
	cmd.AddCommand(&cobra.Command{Use: "invoke PAGE ACTION [JSON]", Args: cobra.RangeArgs(2, 3), Short: "Invoke a named page handler and await its result", RunE: func(c *cobra.Command, args []string) error {
		var input any
		if len(args) == 3 {
			if err := json.Unmarshal([]byte(args[2]), &input); err != nil {
				return fmt.Errorf("invalid action JSON: %w", err)
			}
		}
		return invoke(c, args[0], args[1], input)
	}})
	cmd.AddCommand(&cobra.Command{Use: "shape-only PAGE [on|off|toggle]", Args: cobra.RangeArgs(1, 2), Short: "Toggle the page's single-Shape-button view", RunE: func(c *cobra.Command, args []string) error {
		var input any
		if len(args) == 2 {
			switch args[1] {
			case "on":
				input = true
			case "off":
				input = false
			case "toggle":
			default:
				return fmt.Errorf("expected on, off or toggle")
			}
		}
		return invoke(c, args[0], "outpost.shape-only", input)
	}})
	return cmd
}
