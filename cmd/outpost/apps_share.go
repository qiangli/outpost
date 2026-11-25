package main

import (
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
	fc, err := conf.LoadClientFile(path)
	if err != nil {
		return shareTarget{}, err
	}
	return shareTargetFromConfig(fc)
}

func printShareJSON(w io.Writer, value any) error { return json.NewEncoder(w).Encode(value) }

func trimShareAudience(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func appsShareCmd() *cobra.Command {
	var circles, groups, emails []string
	var asJSON, dryRun bool
	cmd := &cobra.Command{Use: "share APP", Short: "Add one audience rule to an app", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		rule := portal.HostShareRule{
			App:     args[0],
			Circles: trimShareAudience(circles),
			Groups:  trimShareAudience(groups),
			Emails:  trimShareAudience(emails),
		}
		if len(rule.Circles)+len(rule.Groups)+len(rule.Emails) == 0 {
			return errors.New("at least one of --circle, --group, or --email is required")
		}
		for _, audience := range []struct {
			flag   string
			values []string
		}{{"circle", rule.Circles}, {"group", rule.Groups}, {"email", rule.Emails}} {
			if cmd.Flags().Changed(audience.flag) && len(audience.values) == 0 {
				return fmt.Errorf("--%s requires a non-empty value", audience.flag)
			}
		}
		t, err := loadShareTarget()
		if err != nil {
			return err
		}
		if dryRun {
			endpoint := portal.HostSharesURL(t.endpoint, t.host)
			if asJSON {
				return printShareJSON(cmd.OutOrStdout(), struct {
					DryRun bool   `json:"dry_run"`
					Method string `json:"method"`
					URL    string `json:"url"`
					portal.HostShareRule
				}{DryRun: true, Method: "POST", URL: endpoint, HostShareRule: rule})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "would share %s with circles %v, groups %v, emails %v at %s\n",
				rule.App, rule.Circles, rule.Groups, rule.Emails, endpoint)
			return nil
		}
		share, _, err := portal.CreateHostShare(cmd.Context(), t.endpoint, t.token, t.host, rule)
		if err != nil {
			return err
		}
		if asJSON {
			return printShareJSON(cmd.OutOrStdout(), map[string]any{"share": share})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "shared %s with circles %v, groups %v, emails %v (id %s)\n",
			share.App, share.Circles, share.Groups, share.Emails, share.ID)
		return nil
	}}
	cmd.Flags().StringSliceVar(&circles, "circle", nil, "Member circle (repeatable)")
	cmd.Flags().StringSliceVar(&groups, "group", nil, "Owner group (repeatable)")
	cmd.Flags().StringSliceVar(&emails, "email", nil, "Mutual member email (repeatable)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show the request without sending it")
	return cmd
}

func appsUnshareCmd() *cobra.Command {
	var asJSON, dryRun bool
	cmd := &cobra.Command{Use: "unshare SHARE_ID", Short: "Remove an app grant rule by ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id := strings.TrimSpace(args[0])
		if id == "" {
			return errors.New("SHARE_ID is required")
		}
		t, err := loadShareTarget()
		if err != nil {
			return err
		}
		if dryRun {
			endpoint := portal.HostSharesURL(t.endpoint, t.host) + "/" + url.PathEscape(id)
			if asJSON {
				return printShareJSON(cmd.OutOrStdout(), map[string]any{"dry_run": true, "id": id, "method": "DELETE", "url": endpoint})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "would delete share %s at %s\n", id, endpoint)
			return nil
		}
		if err := portal.DeleteHostShare(cmd.Context(), t.endpoint, t.token, t.host, id); err != nil {
			return err
		}
		if asJSON {
			return printShareJSON(cmd.OutOrStdout(), map[string]any{"ok": true, "id": id})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed share %s\n", id)
		return nil
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show the request without sending it")
	return cmd
}

func appsSharesCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "shares [APP]", Short: "List app grant rules", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", s.App,
				strings.Join(s.Circles, ","), strings.Join(s.Groups, ","), strings.Join(s.Emails, ","), s.ID)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	return cmd
}
