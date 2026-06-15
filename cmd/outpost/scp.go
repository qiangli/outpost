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
	"crypto/sha256"
	"encoding/hex"
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
	var safe, keepPrevious bool
	var port int
	cmd := &cobra.Command{
		Use:   "scp <src> <dst>",
		Short: "Copy a file to or from a paired host (LAN-direct when possible)",
		Long: `outpost scp [user@]host:src dst   # download remote → local
outpost scp src [user@]host:dst   # upload   local → remote

Drop-in for the system 'scp' command. Probes mDNS first; when the
peer outpost is on the same LAN, trades the cached matrix_elev
cookie at cloudbox for a short-lived peer ticket and dials LAN-
direct. Falls back to the cloudbox-tunneled path otherwise.
Passwordless after the first 'outpost connect'.

Cloudbox is optional: against a host running 'outpost sshd' (or a
daemon with ssh_listen_addr), pass -P <port> to dial plain TCP on
the LAN — OS-password auth, no pairing or internet needed. An
unpaired machine also falls back to this LAN path automatically
(default port 2222).

Rides the SFTP subsystem under the hood (same as modern openssh-scp
since 8.8). Exactly one of src/dst must carry a [user@]host: prefix
— local-to-local and host-to-host copies are not supported.

Upload-only flags:
  --safe            stage to <dst>.new, sha256-verify the stream, then
                    posix-rename to <dst>. The rename swaps inodes
                    atomically — re-execing a freshly-deployed signed
                    binary won't be SIGKILL'd by macOS amfid, which
                    caches code-signatures by inode.
  --keep-previous   before the swap, posix-rename the existing <dst>
                    to <dst>.previous so rollback is a one-command
                    revert. Implies --safe.

Out of scope for v1 (use system scp or run the copy in two steps):
  -r  recursive directory copy
  -p  preserve mtime/mode`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSCP(cmd.Context(), args[0], args[1], safe || keepPrevious, keepPrevious, port)
		},
	}
	cmd.Flags().BoolVar(&safe, "safe", false, "Stage to <dst>.new and posix-rename — amfid-safe binary delivery (upload only)")
	cmd.Flags().BoolVar(&keepPrevious, "keep-previous", false, "Posix-rename existing <dst> to <dst>.previous before the swap; implies --safe")
	cmd.Flags().IntVarP(&port, "port", "P", 0, "Dial the host directly on this TCP port (LAN 'outpost sshd' / ssh_listen_addr; OS-password auth, no cloudbox)")
	return cmd
}

func runSCP(ctx context.Context, srcArg, dstArg string, safe, keepPrevious bool, port int) error {
	src := parseSCPArg(srcArg)
	dst := parseSCPArg(dstArg)
	switch {
	case !src.Remote && !dst.Remote:
		return errors.New("scp: both arguments are local — use `cp` instead")
	case src.Remote && dst.Remote:
		return errors.New("scp: host-to-host copies are not supported (use two invocations through a local staging file)")
	case src.Remote:
		if safe {
			return errors.New("scp: --safe / --keep-previous apply to uploads only")
		}
		return runSCPDownload(ctx, src, dst.Path, port)
	default:
		if safe {
			return runSCPSafeUpload(ctx, src.Path, dst, keepPrevious, port)
		}
		return runSCPUpload(ctx, src.Path, dst, port)
	}
}

// runSCPDownload copies remote → local. Empty local path means write
// to a file named after the remote basename in the current directory
// — mirrors openssh-scp's `scp host:foo .` ergonomics.
func runSCPDownload(ctx context.Context, src scpEndpoint, localPath string, port int) error {
	client, cleanup, err := dialOutpostHost(ctx, src.Host, src.User, port)
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
func runSCPUpload(ctx context.Context, localPath string, dst scpEndpoint, port int) error {
	if strings.TrimSpace(localPath) == "" {
		return errors.New("scp: empty local source path")
	}
	lf, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local %s: %w", localPath, err)
	}
	defer lf.Close()

	client, cleanup, err := dialOutpostHost(ctx, dst.Host, dst.User, port)
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

// runSCPSafeUpload stages the local file to <remote>.new, hashes the
// stream client-side, then sftp.PosixRename's it over <remote>. On
// POSIX filesystems PosixRename swaps inodes atomically; on macOS the
// inode swap is what lets a re-execed signed binary clear amfid's
// per-inode signature cache. The plain Create-and-overwrite path used
// by runSCPUpload keeps the destination inode and SIGKILLs the next
// re-exec silently (exit 137, empty stderr).
//
// When keepPrevious is true, the existing <remote> is PosixRenamed to
// <remote>.previous before the swap — same atomic-rename trick. A
// missing destination is fine; we just skip the snapshot step.
func runSCPSafeUpload(ctx context.Context, localPath string, dst scpEndpoint, keepPrevious bool, port int) error {
	if strings.TrimSpace(localPath) == "" {
		return errors.New("scp --safe: empty local source path")
	}
	lf, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local %s: %w", localPath, err)
	}
	defer lf.Close()

	client, cleanup, err := dialOutpostHost(ctx, dst.Host, dst.User, port)
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
		return errors.New("scp --safe: explicit remote path required (no SFTP-CWD default)")
	}
	// scp --safe foo host:/some/dir/ — write into the dir under the
	// local basename, matching plain scp's behavior.
	if info, serr := sftpCli.Stat(remotePath); serr == nil && info.IsDir() {
		remotePath = path.Join(remotePath, filepath.Base(localPath))
	}

	stagingPath := remotePath + ".new"
	// O_EXCL surfaces a stale .new from a previous failure as an
	// explicit error instead of silently overwriting it.
	rf, err := sftpCli.OpenFile(stagingPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("create remote staging %s: %w (run `outpost ssh %s -- rm -f %q` if a previous --safe run aborted)", stagingPath, err, dst.Host, stagingPath)
	}
	// Best-effort cleanup of the staging file if anything below fails
	// before we successfully rename it into place.
	stagingKept := false
	defer func() {
		if !stagingKept {
			_ = sftpCli.Remove(stagingPath)
		}
	}()

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(rf, hasher), lf)
	if cerr := rf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copy %s -> %s: %w", localPath, stagingPath, err)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))

	if keepPrevious {
		// PosixRename refuses a missing source on most servers; check
		// first so the absence of a prior generation is silent rather
		// than an error.
		if _, serr := sftpCli.Stat(remotePath); serr == nil {
			previousPath := remotePath + ".previous"
			if err := sftpCli.PosixRename(remotePath, previousPath); err != nil {
				if err2 := sftpCli.Rename(remotePath, previousPath); err2 != nil {
					return fmt.Errorf("snapshot %s -> %s: posix-rename failed (%v) and plain rename failed (%w)", remotePath, previousPath, err, err2)
				}
			}
		}
	}

	if err := sftpCli.PosixRename(stagingPath, remotePath); err != nil {
		// Windows SFTP servers reject posix-rename@openssh.com ("Access is
		// denied") — the atomic inode swap isn't supported there. Fall back to a
		// plain rename: SSH_FXP_RENAME refuses an existing target on those
		// servers, so remove the destination first (loses atomicity, the best
		// available when posix-rename is unsupported).
		_ = sftpCli.Remove(remotePath)
		if err2 := sftpCli.Rename(stagingPath, remotePath); err2 != nil {
			return fmt.Errorf("rename %s -> %s: posix-rename failed (%v) and plain rename failed (%w)", stagingPath, remotePath, err, err2)
		}
	}
	stagingKept = true

	fmt.Fprintf(os.Stderr, "outpost scp --safe: copied %d bytes %s -> %s:%s (sha256=%s)\n", n, localPath, dst.Host, remotePath, digest)
	return nil
}
