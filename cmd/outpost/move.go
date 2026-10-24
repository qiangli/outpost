package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// moveParams carries `outpost move`'s three flags. --to and --code are
// required; --name falls back to the machine hostname like register's.
type moveParams struct {
	To   string
	Code string
	Name string
}

// moveDeps abstracts everything `outpost move` touches outside its own
// file — the existing cluster-leave path, the register exchange, the
// old-server record delete, and the post-pairing restart — so runMove's
// ordering (leave → re-pair → delete, land orphan) runs under a unit test
// with no live servers and no running daemon.
type moveDeps struct {
	leaveCluster   func(ctx context.Context, fc *conf.FileConfig, w io.Writer) error
	exchange       func(ctx context.Context, serverURL, code, name string) error
	deleteOld      func(ctx context.Context, oldBase, oldToken, oldName string) error
	restartOrStart func(ctx context.Context) error
}

// defaultMoveDeps wires moveDeps to the real implementations: the existing
// `outpost cluster leave --yes` decision logic, the same exchange +
// config merge `outpost register` runs, a best-effort host self-delete on
// the old server, and register's restart-or-install-service tail.
func defaultMoveDeps() moveDeps {
	return moveDeps{
		leaveCluster: func(ctx context.Context, fc *conf.FileConfig, w io.Writer) error {
			return runClusterLeave(ctx, fc, true, w, defaultClusterLeaveDeps())
		},
		exchange: func(ctx context.Context, serverURL, code, name string) error {
			return doExchange(ctx, serverURL, code, name, "", "", "", false, "")
		},
		deleteOld: deleteOldHostRecord,
		restartOrStart: func(ctx context.Context) error {
			// Same tail as `outpost register`: a live daemon restarts to
			// pick up the new pairing, otherwise install the service.
			// A failed restart trigger still counts as handled — the
			// operator can re-apply with `outpost restart`, and we must
			// not start a second daemon over the live one.
			if handled, _ := restartRunningDaemon(ctx); handled {
				return nil
			}
			return activatePairedService(false, false)
		},
	}
}

// moveCmd implements `outpost move` — the host side of a satellite transfer
// (spec section 2). B issuing the invite is B's consent; running this on the
// host is A's. The host leaves A's cluster if it is in one, re-pairs to B's
// server with B's invite, best-effort deletes its own record on A's server,
// and lands as an orphan: no cached cluster join is re-issued, so B adds it
// to B's cluster explicitly if wanted.
func moveCmd() *cobra.Command {
	var p moveParams
	cmd := &cobra.Command{
		Use:   "move",
		Short: "Move this host to another server: leave the cluster, re-pair, land as an orphan",
		Long: `Move this host to a different server (satellite transfer, host side).

Runs, in order:
  1. Leave the current cluster if this host is in one (the existing
     ` + "`outpost cluster leave`" + ` path — cloudbox reclaim for a
     cloud-managed node, local disable either way).
  2. Re-pair to the new server with its invite — the same exchange
     ` + "`outpost register`" + ` runs, service restart included.
  3. Best-effort delete this host's own record on the old server with the
     old token. Host credentials cannot delete records on current
     servers, so this usually reports that the old record is left for
     host prune — which catches anything left.

The host lands as an ORPHAN: a re-pair never re-issues the cached
cluster join, so the new server's owner adds it to a cluster explicitly
with ` + "`outpost cluster join`" + ` when wanted.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(p.To) == "" || strings.TrimSpace(p.Code) == "" {
				return errors.New("move requires --to <server URL> and --code <invite from the new server>")
			}
			if p.Name == "" {
				p.Name = defaultHostName()
			}
			if p.Name == "" {
				return errors.New("--name fall back to the hostname failed (host name resolver returned empty) — pass --name explicitly")
			}
			cfgPath, err := conf.DefaultConfigPath()
			if err != nil {
				return err
			}
			// A missing or unreadable config just means no previous
			// pairing: the move degrades to a fresh register (no leave,
			// no old-record delete) instead of refusing.
			fc, lerr := conf.LoadFile(cfgPath)
			if lerr != nil {
				fmt.Fprintf(os.Stderr, "note: no previous pairing found (%v) — moving as a fresh register.\n", lerr)
				fc = nil
			}
			return runMove(cmd.Context(), cfgPath, fc, p, os.Stdout, defaultMoveDeps())
		},
	}
	cmd.Flags().StringVar(&p.To, "to", "", "New server URL (the satellite this host moves to)")
	cmd.Flags().StringVar(&p.Code, "code", "", "Invite code issued by the new server (its consent to take this host)")
	cmd.Flags().StringVar(&p.Name, "name", "", "Host name on the new server (default: this machine's hostname)")
	return cmd
}

// runMove implements moveCmd's ordering against injected deps. fc is the
// pre-move config (nil when never paired); the old server's base, token and
// name are snapshotted before the exchange overwrites them.
func runMove(ctx context.Context, cfgPath string, fc *conf.FileConfig, p moveParams, w io.Writer, deps moveDeps) error {
	oldBase, oldToken, oldName := "", "", ""
	if fc != nil {
		oldBase = cloudboxHTTPBase(fc)
		oldToken = fc.AccessToken
		oldName = fc.AgentName
	}
	paired := oldBase != "" && oldToken != "" && oldName != ""

	// 1. Leave the current cluster first — while the OLD token still works,
	// so cloudbox can reclaim the node. A control-plane host keeps its
	// hosted plane across a re-pair by design (it mints its own
	// credentials), so it skips the leave like every other re-pair path.
	switch {
	case fc != nil && fc.Cluster.ControlPlaneOn() && (fc.ClusterOn() || fc.Cluster.JoinsPeerPlane()):
		fmt.Fprintln(w, "this host is a cluster control plane: keeping its hosted plane across the move (leave it explicitly first if it should stop hosting).")
	case fc != nil && (fc.ClusterOn() || fc.Cluster.JoinsPeerPlane()):
		fmt.Fprintln(w, "leaving the current cluster before re-pairing...")
		if err := deps.leaveCluster(ctx, fc, w); err != nil {
			fmt.Fprintf(w, "warning: cluster leave reported an error (%v) — continuing with the move; rejoin explicitly later if needed.\n", err)
		}
	default:
		fmt.Fprintln(w, "not in a cluster — skipping leave.")
	}

	// 2. Re-pair to the new server — the same exchange register runs. The
	// merge orphans a plain server-plane member (S498.4): no cached join
	// is re-issued, so the host lands outside any cluster.
	if err := deps.exchange(ctx, p.To, p.Code, p.Name); err != nil {
		return fmt.Errorf("re-pair with the new server failed: %w", err)
	}

	// Register's tail: restart a live daemon so it picks up the new
	// pairing, otherwise install the service.
	if err := deps.restartOrStart(ctx); err != nil {
		return fmt.Errorf("paired, but the post-pairing service step failed (run `outpost restart`): %w", err)
	}

	// 3. Best-effort delete of this host's own record on the OLD server,
	// with the OLD token. A same-server "move" keeps its record.
	switch {
	case !paired:
		fmt.Fprintln(w, "no previous pairing — nothing to delete on an old server.")
	case !serverChanged(oldBase, p.To):
		fmt.Fprintln(w, "same server — keeping this host's record.")
	default:
		if err := deps.deleteOld(ctx, oldBase, oldToken, oldName); err != nil {
			if errors.Is(err, errHostDeleteUnsupported) {
				fmt.Fprintf(w, "note: %v — the old record %q is left for host prune on the old server.\n", err, oldName)
			} else {
				fmt.Fprintf(w, "warning: could not delete the old record %q (%v) — host prune on the old server covers it.\n", oldName, err)
			}
		} else {
			fmt.Fprintf(w, "deleted this host's old record %q on the old server.\n", oldName)
		}
	}

	// Report the landing honestly from the post-exchange config: orphaned
	// (the contract), or still attached for the two member kinds a re-pair
	// deliberately preserves.
	if after, rerr := conf.LoadFile(cfgPath); rerr == nil && after.ClusterOn() {
		fmt.Fprintln(w, "moved: still in a cluster (peer-plane or control-plane membership carries across a re-pair — leave explicitly if that was not intended).")
	} else {
		fmt.Fprintln(w, "moved: this host is in no cluster (orphan) — the new server's owner adds it explicitly with `outpost cluster join` when wanted.")
	}
	return nil
}

// serverChanged reports whether moving to rawURL leaves the server whose
// rendered HTTP base is prevBase. Default ports are normalized so an
// explicit :443 / :80 does not read as a different server.
func serverChanged(prevBase, rawURL string) bool {
	return normalizeServerBase(prevBase) != normalizeServerBase(rawURL)
}

func normalizeServerBase(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "/")
	if rest, ok := strings.CutPrefix(s, "https://"); ok {
		return "https://" + strings.TrimSuffix(rest, ":443")
	}
	if rest, ok := strings.CutPrefix(s, "http://"); ok {
		return "http://" + strings.TrimSuffix(rest, ":80")
	}
	return s
}

// errHostDeleteUnsupported marks the expected case: the old server has no
// host-credential self-delete (DELETE /api/hosts/:host needs a user
// session, which a host does not hold). The record is left for host prune.
var errHostDeleteUnsupported = errors.New("old server has no host-credential self-delete")

// deleteOldHostRecord best-effort deletes this host's own record on the old
// server with the OLD token. 2xx means the record is gone; 401/403/404 mean
// the server refused host credentials (the unsupported case above);
// anything else is a plain error for the caller to report. It only ever
// addresses the caller's own old host name with the ?confirm= guard the
// endpoint requires.
func deleteOldHostRecord(ctx context.Context, oldBase, oldToken, oldName string) error {
	base := strings.TrimRight(strings.TrimSpace(oldBase), "/")
	if base == "" || strings.TrimSpace(oldToken) == "" || strings.TrimSpace(oldName) == "" {
		return errHostDeleteUnsupported
	}
	u := base + "/api/hosts/" + url.PathEscape(oldName) + "?confirm=" + url.QueryEscape(oldName)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+oldToken)
	req.Header.Set("Accept", "application/json")
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w (old server responded %d)", errHostDeleteUnsupported, resp.StatusCode)
	default:
		return fmt.Errorf("old server responded %d", resp.StatusCode)
	}
}
