// `outpost peers help-mint-invite` — bearer-authed mint of a fresh
// one-time pairing code so a HEALTHY outpost can drive re-pairing of
// a broken sibling under the same cloudbox account.
//
// Hits cloudbox `POST /api/v1/hosts/invites` with the local outpost's
// AccessToken. Prints the code + expiry so the operator can pass it
// to `outpost repair register --to <peer> --code <code>` (separate
// step so the operator can audit the mint before exposing it via SSH
// on the broken peer's CLI).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func peersHelpMintInviteCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "help-mint-invite [<broken-name>]",
		Short: "Mint a one-time pairing code via cloudbox so a broken peer can re-pair",
		Long: `Calls cloudbox's bearer-authed mint endpoint
(/api/v1/hosts/invites, scope: host:invite-mint) and prints the
resulting code + expiry. The optional <broken-name> argument is
informational — used only in the printed hint. Codes minted here
are valid against any host name (Exchange's name parameter), so
the operator chooses the name at register time.

Use it like this:

    # On a healthy peer (dragon):
    outpost peers help-mint-invite novicortex
    Code: <code>  (expires in 30m)

    # Then on dragon:
    outpost repair register --to novicortex --code <code>

That second command sshs into the broken peer and runs
'outpost register --code <code> --name novicortex' there, so the
broken peer re-pairs with cloudbox using the cached SSH path —
no operator-in-SPA, no LAN reachability to the SPA browser.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			brokenName := ""
			if len(args) > 0 {
				brokenName = args[0]
			}
			return runPeersHelpMintInvite(cmd.Context(), brokenName, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON {code, expires} instead of human-readable text")
	return cmd
}

type helpMintInviteResp struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

func runPeersHelpMintInvite(ctx context.Context, brokenName string, jsonOut bool) error {
	// We don't need MCP here — the local outpost just needs its
	// own AccessToken to call cloudbox. Read FileConfig directly.
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("config path: %w", err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil || fc == nil {
		return fmt.Errorf("load %s: %w (run `outpost register` first)", cfgPath, err)
	}
	if strings.TrimSpace(fc.AccessToken) == "" || strings.TrimSpace(fc.ServerAddr) == "" {
		return fmt.Errorf("this outpost is not paired with cloudbox; cannot mint invites for siblings")
	}
	base := cloudboxHTTPBase(fc)
	if base == "" {
		return fmt.Errorf("cannot derive cloudbox URL from fc.ServerAddr / fc.Protocol")
	}
	url := strings.TrimRight(base, "/") + "/api/v1/hosts/invites"

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+fc.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mint invite: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cloudbox refused (%d): %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var out helpMintInviteResp
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("decode response: %w (body=%s)", err, body)
	}

	if jsonOut {
		fmt.Println(string(body))
		return nil
	}

	mins := int(time.Until(out.Expires).Minutes())
	fmt.Fprintf(os.Stdout, "Code: %s\n", out.Code)
	fmt.Fprintf(os.Stdout, "Expires: %s  (in ~%d min)\n", out.Expires.Format(time.RFC3339), mins)
	fmt.Fprintln(os.Stderr)
	if brokenName != "" {
		fmt.Fprintf(os.Stderr, "Pass to broken peer with:\n")
		fmt.Fprintf(os.Stderr, "  outpost repair register --to %s --code %s --name %s\n", brokenName, out.Code, brokenName)
	} else {
		fmt.Fprintf(os.Stderr, "Pass to broken peer with:\n")
		fmt.Fprintf(os.Stderr, "  outpost repair register --to <peer> --code %s --name <peer>\n", out.Code)
	}
	return nil
}
