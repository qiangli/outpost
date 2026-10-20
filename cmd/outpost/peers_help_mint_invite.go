// `outpost peers help-mint-invite` — bearer-authed mint of a fresh
// one-time pairing code so a HEALTHY outpost can drive re-pairing of
// a broken sibling under the same cloudbox account.
//
// Hits cloudbox `POST /api/v1/hosts/invites` with the local outpost's
// AccessToken. Prints the code + expiry so the operator can pass it
// to `outpost repair register --to <peer> --code <code>` (separate
// step so the operator can audit the mint before exposing it via SSH
// on the broken peer's CLI).
//
// The command MINTS A REAL INVITE, but its help- prefix reads like
// documentation — an operator (or an agent) running it just to learn
// the usage used to mint a live code as a side effect. So the mint is
// gated behind --yes: without it the command makes no HTTP request at
// all, explains what it would do and how to confirm, and exits
// non-zero.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	var (
		jsonOut bool
		yes     bool
	)
	cmd := &cobra.Command{
		Use:   "help-mint-invite [<broken-name>]",
		Short: "Mint a REAL one-time pairing code via cloudbox (requires --yes)",
		Long: `Mints a REAL, single-use pairing code — this is not a help
page. It calls cloudbox's bearer-authed mint endpoint
(POST /api/v1/hosts/invites, scope: host:invite-mint) with this
outpost's access token and prints the resulting code + expiry.

Because the name reads like documentation, the mint is confirm-gated:
without --yes the command makes NO request, mints nothing, prints what
it would have done, and exits non-zero. Re-run with --yes to actually
mint.

The optional <broken-name> argument is informational — used only in
the printed hint. Codes minted here are valid against any host name
(Exchange's name parameter), so the operator chooses the name at
register time.

Use it like this:

    # On a healthy peer (host-a):
    outpost peers help-mint-invite --yes host-b
    Code: <code>  (expires in 30m)

    # Then on host-a:
    outpost repair register --to host-b --code <code>

That second command sshs into the broken peer and runs
'outpost register --code <code> --name host-b' there, so the
broken peer re-pairs with cloudbox using the cached SSH path —
no operator-in-SPA, no LAN reachability to the SPA browser.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			brokenName := ""
			if len(args) > 0 {
				brokenName = args[0]
			}
			return runPeersHelpMintInvite(cmd.Context(), brokenName, jsonOut, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Actually mint the invite (without this flag the command only prints what it would do)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON {code, expires} instead of human-readable text")
	return cmd
}

type helpMintInviteResp struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

// errHelpMintInviteNeedsConfirm is returned (non-zero exit) when the
// command runs without --yes. Nothing has been minted at that point.
var errHelpMintInviteNeedsConfirm = errors.New("help-mint-invite mints a real invite; re-run with --yes to confirm (nothing was minted)")

func runPeersHelpMintInvite(ctx context.Context, brokenName string, jsonOut, yes bool) error {
	// Confirm gate FIRST, before any config read or network I/O, so
	// running the command to read its usage can never mint a code.
	if !yes {
		peersHelpMintInvitePlan(brokenName)
		return errHelpMintInviteNeedsConfirm
	}

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

// peersHelpMintInvitePlan prints what help-mint-invite WOULD do and how
// to confirm it, then the caller returns errHelpMintInviteNeedsConfirm.
// No HTTP request is made and no code is minted — that is the point.
func peersHelpMintInvitePlan(brokenName string) {
	fmt.Fprintln(os.Stderr, "help-mint-invite MINTS A REAL one-time pairing code on cloudbox.")
	fmt.Fprintln(os.Stderr, "This run made no request and minted nothing.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "With --yes it would:")
	fmt.Fprintln(os.Stderr, "  1. POST /api/v1/hosts/invites (scope host:invite-mint) using this")
	fmt.Fprintln(os.Stderr, "     outpost's saved access token — a live, single-use invite is created.")
	fmt.Fprintln(os.Stderr, "  2. Print the code + expiry so you can drive re-pairing with:")
	if brokenName != "" {
		fmt.Fprintf(os.Stderr, "     outpost repair register --to %s --code <code> --name %s\n", brokenName, brokenName)
	} else {
		fmt.Fprintln(os.Stderr, "     outpost repair register --to <peer> --code <code> --name <peer>")
	}
	fmt.Fprintln(os.Stderr)
	confirm := "outpost peers help-mint-invite --yes"
	if brokenName != "" {
		confirm += " " + brokenName
	}
	fmt.Fprintf(os.Stderr, "To actually mint: %s\n", confirm)
}
