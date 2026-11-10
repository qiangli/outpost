package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/portal"
	"github.com/spf13/cobra"
)

type shareTarget struct{ endpoint, token, host string }

func shareTargetFromConfig(fc *conf.FileConfig) (shareTarget, error) {
	if fc == nil {
		return shareTarget{}, errors.New("outpost has no saved pairing or attachment")
	}
	if len(fc.Attachments) > 0 {
		a := fc.Attachments[0]
		if a.Endpoint == "" || a.Token == "" || a.Host == "" {
			return shareTarget{}, errors.New("first satellite attachment is missing endpoint, token, or host")
		}
		return shareTarget{a.Endpoint, a.Token, a.Host}, nil
	}
	if fc.AgentName == "" || fc.AccessToken == "" {
		return shareTarget{}, errors.New("outpost has no satellite attachment or legacy pairing")
	}
	endpoint := strings.TrimSpace(fc.PortalURL)
	if endpoint == "" {
		endpoint = cloudboxHTTPBase(fc)
	}
	if endpoint == "" {
		return shareTarget{}, errors.New("legacy pairing has no portal URL or server")
	}
	return shareTarget{endpoint, fc.AccessToken, fc.AgentName}, nil
}

func loadShareTarget() (shareTarget, error) {
	path, err := conf.DefaultConfigPath()
	if err != nil {
		return shareTarget{}, err
	}
	fc, err := conf.LoadFile(path)
	if err != nil {
		return shareTarget{}, err
	}
	return shareTargetFromConfig(fc)
}

func printShareJSON(w io.Writer, value any) error { return json.NewEncoder(w).Encode(value) }

func appsShareCmd() *cobra.Command {
	var email string
	var asJSON, dryRun bool
	cmd := &cobra.Command{Use: "share APP", Short: "Share an app with a mutual member", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(email) == "" {
			return errors.New("--email is required")
		}
		t, err := loadShareTarget()
		if err != nil {
			return err
		}
		if dryRun {
			plan := map[string]any{"dry_run": true, "method": "POST", "url": portal.HostSharesURL(t.endpoint, t.host), "app": args[0], "email": email}
			if asJSON {
				return printShareJSON(cmd.OutOrStdout(), plan)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "would share %s with %s at %s\n", args[0], email, plan["url"])
			return nil
		}
		share, _, err := portal.CreateHostShare(cmd.Context(), t.endpoint, t.token, t.host, args[0], email)
		if err != nil {
			return err
		}
		if asJSON {
			return printShareJSON(cmd.OutOrStdout(), map[string]any{"share": share})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "shared %s with %s (id %s)\n", share.App, share.Email, share.ID)
		return nil
	}}
	cmd.Flags().StringVar(&email, "email", "", "Mutual member email")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show the request without sending it")
	return cmd
}

func findShare(ctx context.Context, t shareTarget, app, email string) (*portal.AppShare, error) {
	shares, err := portal.ListHostShares(ctx, t.endpoint, t.token, t.host)
	if err != nil {
		return nil, err
	}
	for _, share := range shares {
		if share.App == app && strings.EqualFold(share.Email, email) {
			return &share, nil
		}
	}
	return nil, nil
}

func appsUnshareCmd() *cobra.Command {
	var email string
	var asJSON, dryRun bool
	cmd := &cobra.Command{Use: "unshare APP", Short: "Remove an app share for a member", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(email) == "" {
			return errors.New("--email is required")
		}
		t, err := loadShareTarget()
		if err != nil {
			return err
		}
		share, err := findShare(cmd.Context(), t, args[0], email)
		if err != nil {
			return err
		}
		id := ""
		if share != nil {
			id = share.ID
		}
		if dryRun {
			plan := map[string]any{"dry_run": true, "app": args[0], "email": email, "id": id}
			if id != "" {
				plan["method"] = "DELETE"
				plan["url"] = portal.HostSharesURL(t.endpoint, t.host) + "/" + url.PathEscape(id)
			}
			if asJSON {
				return printShareJSON(cmd.OutOrStdout(), plan)
			}
			if id == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "already unshared")
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "would delete share %s\n", id)
			}
			return nil
		}
		if id != "" {
			if err := portal.DeleteHostShare(cmd.Context(), t.endpoint, t.token, t.host, id); err != nil {
				return err
			}
		}
		if asJSON {
			return printShareJSON(cmd.OutOrStdout(), map[string]any{"ok": true, "id": id})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "unshared %s from %s\n", args[0], email)
		return nil
	}}
	cmd.Flags().StringVar(&email, "email", "", "Mutual member email")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Resolve the share without deleting it")
	return cmd
}

func appsSharesCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "shares [APP]", Short: "List app shares", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		t, err := loadShareTarget()
		if err != nil {
			return err
		}
		shares, err := portal.ListHostShares(cmd.Context(), t.endpoint, t.token, t.host)
		if err != nil {
			return err
		}
		if len(args) == 1 {
			filtered := make([]portal.AppShare, 0)
			for _, s := range shares {
				if s.App == args[0] {
					filtered = append(filtered, s)
				}
			}
			shares = filtered
		}
		if asJSON {
			return printShareJSON(cmd.OutOrStdout(), map[string]any{"shares": shares})
		}
		for _, s := range shares {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", s.App, s.Email, s.ID)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	return cmd
}
