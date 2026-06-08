// `outpost ssh [user@]<host> [cmd...]` — drop-in `ssh` invocation that
// prefers a LAN-direct connection (with cloudbox-issued peer-ticket
// auth) over the cloudbox-tunneled path. Designed for agentic callers
// that want a passwordless SSH after the first `outpost connect`.
//
// Flow:
//  1. Parse [user@]host.
//  2. mDNS browse for `host` (match on AgentName / AssignedHostname).
//  3. Cookie lifecycle: read cached matrix_elev; if missing, runConnect.
//  4. If a LAN peer with `lan-ssh-ws` endpoint is found:
//     - Trade cookie at cloudbox for a peer ticket.
//     - Verify peer's mDNS-advertised host-key fingerprint after
//     the SSH handshake (defense against LAN MITM that races mDNS).
//     - WSS dial directly to the LAN endpoint with PeerTicket.
//  5. Otherwise fall back to the cloudbox-tunneled path (existing
//     behavior of `outpost ssh-proxy`).
//  6. Run interactive shell or exec the provided command.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
	"github.com/qiangli/outpost/internal/agent/sshclient"
)

// runSSHHost is the entry point for `outpost ssh [user@]<host> [cmd...]`.
// `arg` is the first positional ("user@host" or just "host"); `cmdArgs`
// is anything after that (treated as a remote command to exec; empty
// for interactive shell).
func runSSHHost(ctx context.Context, arg string, cmdArgs []string) error {
	user, host := parseUserAtHost(arg)

	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("locate config: %w", err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if fc == nil || fc.ServerAddr == "" {
		return errors.New("local outpost is not paired with cloudbox yet — run `outpost register` first")
	}
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		return errors.New("no cloudbox bearer cached — re-pair with `outpost register`")
	}

	// Resolve OS user (if not explicit) the same way `outpost connect`
	// does: prefer cloudbox's /api/v1/ssh/hosts report, fall back to
	// $USER. The remote outpost's /auth gate compares against its own
	// OS user, not the caller's $USER.
	if user == "" {
		if hosts, ferr := fetchSSHHosts(ctx, fc.ServerAddr, fc.ServerPort, fc.Protocol, bearer); ferr == nil {
			for _, h := range hosts {
				if strings.EqualFold(h.Host, host) && h.OsUser != "" {
					user = h.OsUser
					break
				}
			}
		}
	}
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USER"))
	}
	if user == "" {
		return errors.New("could not determine OS username; use `outpost ssh user@host`")
	}

	// Ensure we have a cached matrix_elev cookie for this host. If
	// not, run the same elevation flow `outpost connect` does (TTY
	// prompt; --stdin not exposed here because the surrounding tree
	// has its own ergonomics, but the caller can always run `outpost
	// connect --stdin <host>` separately before invoking ssh).
	cookie, _ := conf.ReadSessionCookie(host)
	if strings.TrimSpace(cookie) == "" {
		fmt.Fprintf(os.Stderr, "outpost: %s has no cached elevation; prompting for OS password…\n", host)
		if err := runConnect(ctx, host, user, false, false, 0); err != nil {
			return fmt.Errorf("elevate %s: %w", host, err)
		}
		cookie, _ = conf.ReadSessionCookie(host)
		if strings.TrimSpace(cookie) == "" {
			return errors.New("elevation completed but no cookie was cached")
		}
	}

	// LAN-direct probe. mDNS browse blocks for ~3s; fine for the
	// first connect, slightly chatty for tight loops. The probe is
	// best-effort: any failure (no peer found, no sshws endpoint,
	// no pubkey configured, peer-ticket exchange fails) falls back
	// to the cloudbox-tunneled path so the command always works.
	if client, cleanup, err := dialLANDirect(ctx, fc, bearer, host, user, cookie); err == nil {
		return runRemote(ctx, client, cleanup, host, cmdArgs)
	} else if !errors.Is(err, errLANNotAvailable) {
		// Log non-trivial LAN-direct failures (e.g. peer-ticket
		// exchange returned 5xx) so the operator can see what
		// happened, but still fall through to the tunneled path.
		slog.Info("ssh: LAN-direct attempt failed, falling back to cloudbox tunnel",
			"host", host, "err", err)
	}

	client, cleanup, err := dialCloudboxTunnel(ctx, fc, bearer, host, user, cookie)
	if err != nil {
		return err
	}
	return runRemote(ctx, client, cleanup, host, cmdArgs)
}

// errLANNotAvailable signals that LAN-direct simply doesn't apply
// (no peer found, no sshws endpoint, no pubkey on this side).
// Differentiated from a real error so the caller can fall back
// silently.
var errLANNotAvailable = errors.New("lan-direct not available")

// dialLANDirect attempts the LAN-direct path. Returns errLANNotAvailable
// when the path doesn't apply (caller falls back without a warning);
// returns a real error when the path was applicable but failed (caller
// falls back and logs).
func dialLANDirect(
	ctx context.Context,
	fc *conf.FileConfig,
	bearer, host, user, cookie string,
) (*sshclient.Client, func(), error) {
	// Without a cloudbox base URL we can't trade the cookie for a
	// peer ticket; skip silently.
	cbBase := cloudboxHTTPBase(fc)
	if cbBase == "" {
		return nil, nil, errLANNotAvailable
	}

	browseCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	peers, _ := discovery.Browse(browseCtx, discovery.BrowseOptions{Timeout: 3 * time.Second})
	cancel()
	peer := findMatchingPeer(peers, host)
	if peer == nil {
		return nil, nil, errLANNotAvailable
	}
	ep := peer.FirstEndpoint(discovery.EndpointLANSSHWS)
	if ep.Host == "" || ep.Port == 0 {
		return nil, nil, errLANNotAvailable
	}

	ticket, err := exchangePeerTicket(ctx, cbBase, bearer, cookie, host, "ssh")
	if err != nil {
		return nil, nil, fmt.Errorf("peer-ticket exchange: %w", err)
	}

	wsURL := fmt.Sprintf("ws://%s/ssh", ep.HostPort())
	wsConn, werr := sshclient.DialWS(ctx, sshclient.DialOptions{
		WSURL:      wsURL,
		PeerTicket: ticket,
		Host:       host,
	})
	if werr != nil {
		return nil, nil, fmt.Errorf("lan-direct dial %s: %w", ep.HostPort(), werr)
	}

	// Host-key fingerprint check: mDNS advertised id=SHA256:<peer
	// host-key hash>. After the SSH handshake reveals the actual
	// host key, compare. Mismatch = abort (likely an attacker on the
	// LAN racing the mDNS announce). For a peer that didn't
	// advertise an id, fall through to the TOFU known_hosts policy.
	advFP := string(peer.ID)
	hostKeyCB := makeFingerprintCheckingCallback(host, advFP)

	cli, err := sshclient.Dial(ctx, sshclient.Config{
		Transport:       sshclient.AsNetConn(ctx, wsConn),
		HostAlias:       sshclient.HostAliasForHost(host),
		User:            user,
		HostKeyCallback: hostKeyCB,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ssh handshake to %s (lan-direct): %w", host, err)
	}

	// Reachability ledger so `outpost peers list` shows the LAN-direct
	// edge instead of the cloudbox-tunneled one when the LAN path won.
	recordReachabilityEdge(conf.SSHTarget{Name: host, Host: ep.Host, Port: ep.Port, Direct: true}, time.Now())

	cleanup := func() { _ = cli.Close() }
	return cli, cleanup, nil
}

// dialCloudboxTunnel is the fallback path — identical to what
// `outpost ssh-proxy` does today, just with the cookie already known
// (we minted it above if it was missing) so no in-band recovery is
// needed.
func dialCloudboxTunnel(
	ctx context.Context,
	fc *conf.FileConfig,
	bearer, host, user, cookie string,
) (*sshclient.Client, func(), error) {
	wsURL, err := sshclient.BuildWSURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, host)
	if err != nil {
		return nil, nil, err
	}
	wsConn, werr := sshclient.DialWS(ctx, sshclient.DialOptions{
		WSURL:  wsURL,
		Bearer: bearer,
		Cookie: cookie,
		Host:   host,
		OnElevate: func(c context.Context, h string) (string, error) {
			if !haveTTY() {
				return "", errors.New("no TTY for password prompt")
			}
			fmt.Fprintf(os.Stderr, "outpost: %s requires Connect; prompting for OS password…\n", h)
			if err := runConnect(c, h, "", false, false, 0); err != nil {
				return "", err
			}
			fresh, _ := conf.ReadSessionCookie(h)
			return fresh, nil
		},
	})
	if werr != nil {
		return nil, nil, werr
	}
	knownHostsPath, err := conf.KnownHostsPath()
	if err != nil {
		return nil, nil, err
	}
	hostKeyCB, err := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(host))
	if err != nil {
		return nil, nil, err
	}
	cli, err := sshclient.Dial(ctx, sshclient.Config{
		Transport:       sshclient.AsNetConn(ctx, wsConn),
		HostAlias:       sshclient.HostAliasForHost(host),
		User:            user,
		HostKeyCallback: hostKeyCB,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ssh handshake to %s: %w", host, err)
	}
	recordReachabilityEdge(conf.SSHTarget{Name: host, Host: host, Direct: false}, time.Now())
	cleanup := func() { _ = cli.Close() }
	return cli, cleanup, nil
}

// runRemote dispatches to Shell or Exec based on whether the caller
// supplied a remote command.
func runRemote(ctx context.Context, client *sshclient.Client, cleanup func(), host string, cmdArgs []string) error {
	defer cleanup()
	if len(cmdArgs) == 0 {
		exit, err := client.Shell(ctx, sshclient.ShellOptions{
			Stdin:  os.Stdin,
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		})
		if err != nil {
			return fmt.Errorf("shell on %s: %w", host, err)
		}
		if exit != 0 {
			os.Exit(exit)
		}
		return nil
	}
	res, err := client.Exec(ctx, sshclient.ExecOptions{
		Command: strings.Join(cmdArgs, " "),
		Stdin:   os.Stdin,
	})
	if err != nil {
		return fmt.Errorf("exec on %s: %w", host, err)
	}
	if len(res.Stdout) > 0 {
		_, _ = os.Stdout.Write(res.Stdout)
	}
	if len(res.Stderr) > 0 {
		_, _ = os.Stderr.Write(res.Stderr)
	}
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

// parseUserAtHost splits "user@host" into (user, host). Returns
// ("", "host") when no `@` is present. Leading/trailing whitespace
// is trimmed from both parts.
func parseUserAtHost(s string) (string, string) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, "@")
	if i < 0 {
		return "", s
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
}

// findMatchingPeer returns the first peer whose AgentName or
// AssignedHostname matches host (case-insensitive). Same matching
// rule lookupDiscoveredPeer uses, factored out here so the runtime
// path doesn't share its "no LAN peer" error message (which is
// designed for `outpost ssh add --from-peer` failures).
func findMatchingPeer(peers []discovery.Peer, host string) *discovery.Peer {
	name := strings.TrimSpace(strings.ToLower(host))
	for i := range peers {
		p := &peers[i]
		if strings.EqualFold(p.AgentName, name) || strings.EqualFold(p.AssignedHostname, name) {
			return p
		}
	}
	return nil
}

// makeFingerprintCheckingCallback returns an SSH HostKeyCallback that
// verifies the actual host key's SHA256 fingerprint matches what mDNS
// advertised. Empty advFP means "no advertised fingerprint" — fall
// back to TOFU via known_hosts (this happens with peers running
// pre-discovery outposts).
func makeFingerprintCheckingCallback(host, advFP string) ssh.HostKeyCallback {
	if strings.TrimSpace(advFP) == "" {
		// Best-effort fall-back: TOFU. This is the same callback the
		// cloudbox-tunneled path uses, so behavior here matches the
		// existing trust model for peers we can't validate via mDNS.
		knownHostsPath, _ := conf.KnownHostsPath()
		cb, _ := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(host))
		return cb
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if fp != advFP {
			return fmt.Errorf("ssh host-key fingerprint mismatch for %s: mDNS=%s actual=%s (possible LAN MITM; aborting LAN-direct)", host, advFP, fp)
		}
		return nil
	}
}

// peerTicketResponse is the wire shape of the cloudbox endpoint's
// success response. Kept tiny on purpose — the outpost only needs the
// ticket string itself.
type peerTicketResponse struct {
	Ticket string `json:"ticket"`
}

// exchangePeerTicket trades the local outpost's matrix_elev cookie at
// cloudbox's `/api/v1/ssh/peer-ticket` for a short-lived JWT scoped to
// `host` and `scope`. The cookie itself never leaves this exchange —
// only the derived ticket is presented to the peer.
//
// A 404 from cloudbox (endpoint not deployed yet) is treated as
// errLANNotAvailable so the caller can fall back to the tunnel.
func exchangePeerTicket(ctx context.Context, cbBase, bearer, cookie, host, scope string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"host":  host,
		"scope": []string{scope},
	})
	u := strings.TrimRight(cbBase, "/") + "/api/v1/ssh/peer-ticket"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Cookie", "matrix_elev="+cookie)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		// Endpoint not deployed yet on cloudbox; tunnel-fallback.
		return "", errLANNotAvailable
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cloudbox returned %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	var out peerTicketResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode peer-ticket response: %w", err)
	}
	if strings.TrimSpace(out.Ticket) == "" {
		return "", errors.New("cloudbox returned empty ticket")
	}
	return out.Ticket, nil
}
