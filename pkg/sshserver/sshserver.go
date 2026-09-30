// Package sshserver exposes outpost's standalone LAN SSH server to other
// binaries in this module.
//
// The protocol implementation remains in internal/agent. This package is a
// deliberately small adapter for bootstrap users: it authenticates the
// daemon's OS user with authorized_keys, and serves exec, SFTP, and loopback
// direct-tcpip forwarding over a caller-owned listener.
package sshserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent"
)

// Config contains the install-time identity and authorization material for a
// standalone SSH server. The host key is also the identity presented to SSH
// clients; it should be persisted by the installer so it remains stable.
type Config struct {
	// AuthorizedKeysPath is the install-time authorized_keys file. An empty
	// path uses the current user's ~/.ssh/authorized_keys.
	AuthorizedKeysPath string
	// HostKey is the server's persistent SSH host identity.
	HostKey ssh.Signer
}

// Serve accepts SSH connections from ln until ctx is canceled or the
// listener fails. The listener is closed when ctx is canceled. Authentication
// and authorization are intentionally fixed to outpost's LAN policy: the
// submitted SSH username must identify the current OS user and the key must
// be present in AuthorizedKeysPath. Exec, SFTP, and loopback direct-tcpip
// forwarding are enabled.
func Serve(ctx context.Context, ln net.Listener, cfg Config) error {
	if ctx == nil {
		return errors.New("sshserver: nil context")
	}
	if ln == nil {
		return errors.New("sshserver: nil listener")
	}
	if cfg.HostKey == nil {
		return errors.New("sshserver: nil host key")
	}
	return agent.ServeLANSSH(ctx, ln, agent.Deps{
		SSHHostKey:            cfg.HostKey,
		SSHAuthorizedKeysFile: strings.TrimSpace(cfg.AuthorizedKeysPath),
		SSHAllowLocalForward:  true,
		SSHAllowRemoteForward: true,
		SFTPEnabled:           true,
	})
}

// ListenAndServe listens on address and serves until ctx is canceled. The
// address may be a normal host:port or a bare port such as "2222". The
// listener is owned and closed by this function.
func ListenAndServe(ctx context.Context, address string, cfg Config) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return errors.New("sshserver: empty listen address")
	}
	if !strings.Contains(address, ":") {
		address = ":" + address
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("sshserver: listen %s: %w", address, err)
	}
	return Serve(ctx, ln, cfg)
}
