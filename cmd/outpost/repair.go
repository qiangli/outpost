// `outpost repair` — peer-assisted recovery flows.
//
// Wave 3A.1 ships two narrow recipes that compose existing primitives
// (`outpost ssh exec`, `outpost ssh sftp get`, `outpost upgrade --local`):
//
//	outpost repair cloudbox-url --from <peer>
//	  Prints the peer's cloudbox base URL so the operator can
//	  re-register against the right server when the local config is
//	  damaged. Tier-2 (peer must be a configured SSH target).
//
//	outpost repair binary --from <peer> [--out PATH]
//	  Fetches the peer's outpost binary, validates by exec'ing
//	  `<candidate> version --json`, swaps it in atomically via the
//	  existing upgrade Worker, restarts the daemon. Tier-2.
//
// Both flows go through the SSH client (`outpost ssh exec` /
// `outpost ssh sftp get`), so the peer must already be an `outpost ssh
// add`-ed alias. For LAN-direct repair after a corrupt config, add a
// `--direct` target by IP first.
//
// Phase 2 (Wave 3A.2) adds `outpost repair register` for peer-relayed
// re-registration with cloudbox.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func repairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Peer-assisted recovery when cloudbox / local config / local binary is in a bad state",
	}
	cmd.AddCommand(
		repairCloudboxURLCmd(),
		repairBinaryCmd(),
		repairRemoteBinaryCmd(),
		repairRegisterCmd(),
	)
	return cmd
}

func repairCloudboxURLCmd() *cobra.Command {
	var fromFlag string
	cmd := &cobra.Command{
		Use:   "cloudbox-url",
		Short: "Ask a peer outpost for the cloudbox URL it's paired with",
		Long: `Runs 'outpost config show' on the peer via SSH and prints the
cloudbox base URL it's using. Useful when this outpost's local
config is corrupt or this outpost was never paired and the operator
needs to know which cloudbox to 'outpost register' against.

The peer must already be a configured SSH target (run
'outpost ssh add' first).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fromFlag == "" {
				return fmt.Errorf("--from <peer> is required (the SSH target alias of a healthy peer)")
			}
			return runRepairCloudboxURL(cmd.Context(), fromFlag)
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "SSH target alias of the peer to ask. Required.")
	return cmd
}

func runRepairCloudboxURL(ctx context.Context, peerName string) error {
	// Compose: ssh exec on the peer; parse out the cloudbox URL.
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()
	var res sshExecResult
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
		"name":            peerName,
		"command":         "outpost config show --json 2>/dev/null || outpost config show",
		"timeout_seconds": 30,
	}, &res); err != nil {
		return err
	}
	if res.ExitCode != 0 {
		fmt.Fprint(os.Stderr, res.Stderr)
		return fmt.Errorf("peer's `outpost config show` exited with %d", res.ExitCode)
	}
	fmt.Println("Peer config from", peerName+":")
	fmt.Println(res.Stdout)
	return nil
}

func repairBinaryCmd() *cobra.Command {
	var (
		fromFlag       string
		outFlag        string
		applyFlag      bool
		remotePathFlag string
	)
	cmd := &cobra.Command{
		Use:   "binary",
		Short: "Fetch a fresh outpost binary from a healthy peer (and optionally apply via upgrade)",
		Long: `Locates the peer's outpost binary path, sftp-gets it to a
local candidate file, then (when --apply is passed) runs
'outpost upgrade --local <candidate>' to atomically swap it in.

The peer must already be a configured SSH target (run
'outpost ssh add' first). The peer's outpost must be running with
SFTPEnabled (default on).

By default the candidate is written to ~/.cache/outpost/repair/outpost
so the operator can inspect / sha256sum before applying.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fromFlag == "" {
				return fmt.Errorf("--from <peer> is required (the SSH target alias of a healthy peer)")
			}
			return runRepairBinary(cmd.Context(), fromFlag, outFlag, remotePathFlag, applyFlag)
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "SSH target alias of the peer to fetch from. Required.")
	cmd.Flags().StringVar(&outFlag, "out", "", "Local candidate path (default: ~/.cache/outpost/repair/outpost)")
	cmd.Flags().StringVar(&remotePathFlag, "remote-path", "", "Override the remote binary path (default: auto-detected via 'which outpost')")
	cmd.Flags().BoolVar(&applyFlag, "apply", false, "After fetching, run 'outpost upgrade --local <candidate>' to swap + restart")
	return cmd
}

func runRepairBinary(ctx context.Context, peerName, outPath, remotePath string, apply bool) error {
	// Step 1: resolve the remote binary path. Auto-detect by running
	// `which outpost || readlink /proc/self/exe` on the peer when the
	// operator didn't override.
	session, err := dialMCP(ctx)
	if err != nil {
		return err
	}
	defer session.close()

	if remotePath == "" {
		var probe sshExecResult
		if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
			"name":            peerName,
			"command":         "command -v outpost || echo ~/bin/outpost",
			"timeout_seconds": 15,
		}, &probe); err != nil {
			return fmt.Errorf("locate peer binary: %w", err)
		}
		if probe.ExitCode != 0 {
			return fmt.Errorf("peer's binary probe exited %d: %s", probe.ExitCode, probe.Stderr)
		}
		remotePath = ""
		for _, line := range splitLines(probe.Stdout) {
			line = trimSpace(line)
			if line != "" {
				remotePath = line
				break
			}
		}
		if remotePath == "" {
			return fmt.Errorf("could not auto-detect peer binary path; pass --remote-path")
		}
	}
	fmt.Fprintf(os.Stderr, "Peer binary located at %s on %s.\n", remotePath, peerName)

	// Step 2: default local candidate path.
	if outPath == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		dir := filepath.Join(cache, "outpost", "repair")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		outPath = filepath.Join(dir, "outpost")
	}

	// Step 3: SFTP get. We don't have a structured MCP tool for sftp
	// get that returns the bytes; the existing CLI surface
	// `outpost ssh sftp get` is interactive-shaped. Use the daemon's
	// outpost_ssh_exec to base64 the file, then decode locally.
	// (Wave 3A.2 may add a proper sftp-get-bytes MCP tool.)
	var fetch sshExecResult
	if err := session.callTool(ctx, "outpost_ssh_exec", map[string]any{
		"name":            peerName,
		"command":         "base64 < " + shellQuote(remotePath),
		"timeout_seconds": 600,
		"max_stdout":      256 * 1024 * 1024, // 256 MiB — outpost binaries are ~100 MiB
	}, &fetch); err != nil {
		return fmt.Errorf("fetch binary via base64 exec: %w", err)
	}
	if fetch.ExitCode != 0 {
		return fmt.Errorf("peer's base64 exited %d: %s", fetch.ExitCode, fetch.Stderr)
	}
	// base64 from the peer may have line wrapping; strip whitespace
	// before decoding.
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, fetch.Stdout)
	bin, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return fmt.Errorf("decode fetched binary: %w", err)
	}
	if err := os.WriteFile(outPath, bin, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Fprintf(os.Stderr, "Fetched %d bytes → %s\n", len(bin), outPath)

	// Step 4: (optional) apply via the existing upgrade flow.
	// We invoke `outpost upgrade --local <path>` as a subprocess so we
	// reuse the cobra RunE there verbatim (probe + stage + swap +
	// restart). The current binary's path comes from os.Executable().
	if apply {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate own binary: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Applying via '%s upgrade --local %s' ...\n", self, outPath)
		return execSelfUpgrade(ctx, self, outPath)
	}
	fmt.Fprintf(os.Stderr, "Done. Inspect with:\n  %s version --json\n  sha256sum %s\nThen apply with:\n  outpost upgrade --local %s\n",
		outPath, outPath, outPath)
	return nil
}

// execSelfUpgrade shells out to `<self> upgrade --local <candidate>`.
// Used by `repair binary --apply`. We do not import the upgrade
// subcommand directly to avoid intertwining the two CLI flows.
func execSelfUpgrade(ctx context.Context, self, candidate string) error {
	cmd := exec.CommandContext(ctx, self, "upgrade", "--local", candidate)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- helpers ---

func splitLines(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func shellQuote(s string) string {
	// Conservative single-quote escape.
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
		} else {
			out += string(r)
		}
	}
	out += "'"
	return out
}
