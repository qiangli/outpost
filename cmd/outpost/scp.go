// `outpost scp [user@]host:src dst`  — download remote file to local.
// `outpost scp src [user@]host:dst`  — upload local file to remote.
//
// Drop-in for the `scp` command (single file only — see "Out of scope"
// at the bottom). Reuses the same dial path as `outpost ssh` so
// LAN-direct + peer-ticket auth kicks in automatically when the peer
// is reachable on mDNS; otherwise falls back to the cloudbox tunnel.
// Passwordless after the first `outpost connect`.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// scpEndpoint is the result of parsing one positional scp argument.
// Remote == false means the argument is a plain local path; Remote ==
// true means User/Host/Path are populated from the `[user@]host:path`
// shape. Exactly one of the two positional args must be Remote (no
// local-to-local, no host-to-host).
type scpEndpoint struct {
	Remote bool
	User   string
	Host   string
	Path   string // either local path or remote path, depending on Remote
}

// parseSCPArg classifies one positional arg as local or remote. The
// rule mirrors openssh-scp: a `:` is a remote separator only when
// no `/` appears before it. So `./foo:bar` is local (slash first),
// `foo:bar` is remote, `host:/abs/path` is remote.
//
// On remote, the part before `:` is `[user@]host` — same parser
// outpost ssh uses for its first positional.
func parseSCPArg(arg string) scpEndpoint {
	colon := strings.Index(arg, ":")
	slash := strings.Index(arg, "/")
	if colon < 0 || (slash >= 0 && slash < colon) {
		return scpEndpoint{Remote: false, Path: arg}
	}
	hostPart := arg[:colon]
	path := arg[colon+1:]
	user, host := parseUserAtHost(hostPart)
	return scpEndpoint{
		Remote: true,
		User:   user,
		Host:   host,
		Path:   path,
	}
}

func scpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scp <src> <dst>",
		Short: "Copy a file to or from a paired host (LAN-direct when possible)",
		Long: `outpost scp [user@]host:src dst   # download remote → local
outpost scp src [user@]host:dst   # upload   local → remote

Drop-in for the system 'scp' command. Probes mDNS first; when the
peer outpost is on the same LAN, trades the cached matrix_elev
cookie at cloudbox for a short-lived peer ticket and dials LAN-
direct. Falls back to the cloudbox-tunneled path otherwise.
Passwordless after the first 'outpost connect'.

Rides the SFTP subsystem under the hood (same as modern openssh-scp
since 8.8). Exactly one of src/dst must carry a [user@]host: prefix
— local-to-local and host-to-host copies are not supported.

Out of scope for v1 (use system scp or run the copy in two steps):
  -r  recursive directory copy
  -p  preserve mtime/mode
  -P  custom port (LAN-direct uses the advertised sshws port; the
      tunneled path uses cloudbox's HTTPS port)`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSCP(cmd.Context(), args[0], args[1])
		},
	}
}

func runSCP(ctx context.Context, srcArg, dstArg string) error {
	src := parseSCPArg(srcArg)
	dst := parseSCPArg(dstArg)
	switch {
	case !src.Remote && !dst.Remote:
		return errors.New("scp: both arguments are local — use `cp` instead")
	case src.Remote && dst.Remote:
		return errors.New("scp: host-to-host copies are not supported (use two invocations through a local staging file)")
	case src.Remote:
		return runSCPDownload(ctx, src, dst.Path)
	default:
		return runSCPUpload(ctx, src.Path, dst)
	}
}

// runSCPDownload copies remote → local. Empty local path means write
// to a file named after the remote basename in the current directory
// — mirrors openssh-scp's `scp host:foo .` ergonomics.
func runSCPDownload(ctx context.Context, src scpEndpoint, localPath string) error {
	client, cleanup, err := dialOutpostHost(ctx, src.Host, src.User)
	if err != nil {
		return err
	}
	defer cleanup()

	sftpCli, err := client.SFTP()
	if err != nil {
		return fmt.Errorf("open sftp subsystem on %s: %w", src.Host, err)
	}
	defer sftpCli.Close()

	rf, err := sftpCli.Open(src.Path)
	if err != nil {
		return fmt.Errorf("open remote %s: %w", src.Path, err)
	}
	defer rf.Close()

	if strings.TrimSpace(localPath) == "" || localPath == "." {
		localPath = filepath.Base(src.Path)
	}
	// If localPath names an existing directory, write into it under
	// the remote basename — matches `scp host:foo .` behavior.
	if info, serr := os.Stat(localPath); serr == nil && info.IsDir() {
		localPath = filepath.Join(localPath, filepath.Base(src.Path))
	}
	out, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create local %s: %w", localPath, err)
	}
	defer out.Close()

	n, err := io.Copy(out, rf)
	if err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src.Path, localPath, err)
	}
	fmt.Fprintf(os.Stderr, "outpost scp: copied %d bytes %s:%s -> %s\n", n, src.Host, src.Path, localPath)
	return nil
}

// runSCPUpload copies local → remote. Empty remote path falls back
// to the local basename in the SFTP working directory (typically the
// remote user's home).
func runSCPUpload(ctx context.Context, localPath string, dst scpEndpoint) error {
	if strings.TrimSpace(localPath) == "" {
		return errors.New("scp: empty local source path")
	}
	lf, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local %s: %w", localPath, err)
	}
	defer lf.Close()

	client, cleanup, err := dialOutpostHost(ctx, dst.Host, dst.User)
	if err != nil {
		return err
	}
	defer cleanup()

	sftpCli, err := client.SFTP()
	if err != nil {
		return fmt.Errorf("open sftp subsystem on %s: %w", dst.Host, err)
	}
	defer sftpCli.Close()

	remotePath := dst.Path
	if strings.TrimSpace(remotePath) == "" {
		remotePath = filepath.Base(localPath)
	}
	// If the remote path resolves to an existing directory, write
	// into it under the local basename — `scp foo host:/some/dir/`
	// behavior. Best-effort: SFTP Stat can fail for permission
	// reasons we don't want to mistake for "not a dir."
	if info, serr := sftpCli.Stat(remotePath); serr == nil && info.IsDir() {
		remotePath = path.Join(remotePath, filepath.Base(localPath))
	}
	rf, err := sftpCli.Create(remotePath)
	if err != nil {
		return fmt.Errorf("create remote %s: %w", remotePath, err)
	}
	defer rf.Close()

	n, err := io.Copy(rf, lf)
	if err != nil {
		return fmt.Errorf("copy %s -> %s: %w", localPath, remotePath, err)
	}
	fmt.Fprintf(os.Stderr, "outpost scp: copied %d bytes %s -> %s:%s\n", n, localPath, dst.Host, remotePath)
	return nil
}

