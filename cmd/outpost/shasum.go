// `outpost shasum [user@]host:path` — print the sha256 of a remote
// file in `shasum -a 256` output format ("<hex>  <path>"), so it
// pipes / diffs against the system tool cleanly:
//
//   diff <(outpost shasum host:/opt/bin/foo | awk '{print $1}') \
//        <(shasum -a 256 ./foo                | awk '{print $1}')
//
// Rides the same SFTP subsystem `outpost scp` uses; stream-reads the
// remote file into sha256.New() so no temporary download is materialized.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func shasumCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shasum [user@]host:path",
		Short: "Print the sha256 of a remote file (shasum -a 256 format)",
		Long: `outpost shasum [user@]host:path

Streams the remote file through sha256 over the SFTP subsystem and
prints "<hex>  <path>" — same shape system 'shasum -a 256' emits, so
piping or diffing against a local hash is one shell line.

Reuses the same LAN-direct + cloudbox-fallback dial path as
'outpost ssh' / 'outpost scp'; passwordless after the first
'outpost connect'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShasum(cmd.Context(), args[0])
		},
	}
}

func runShasum(ctx context.Context, arg string) error {
	ep := parseSCPArg(arg)
	if !ep.Remote {
		return errors.New("shasum: argument must be [user@]host:path (use system 'shasum -a 256' for local files)")
	}
	if ep.Path == "" {
		return errors.New("shasum: empty remote path")
	}

	client, cleanup, err := dialOutpostHost(ctx, ep.Host, ep.User)
	if err != nil {
		return err
	}
	defer cleanup()

	sftpCli, err := client.SFTP()
	if err != nil {
		return fmt.Errorf("open sftp subsystem on %s: %w", ep.Host, err)
	}
	defer sftpCli.Close()

	rf, err := sftpCli.Open(ep.Path)
	if err != nil {
		return fmt.Errorf("open remote %s: %w", ep.Path, err)
	}
	defer rf.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, rf); err != nil {
		return fmt.Errorf("hash remote %s: %w", ep.Path, err)
	}
	fmt.Printf("%s  %s\n", hex.EncodeToString(hasher.Sum(nil)), ep.Path)
	return nil
}
