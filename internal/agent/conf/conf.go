// Package conf holds the matrix-agent runtime configuration.
package conf

import (
	"fmt"
	"os"
	"strconv"
)

// Config drives the matrix-agent binary. Env vars are the primary source;
// CLI flags override env.
type Config struct {
	// Local loopback HTTP server (cloudbox reaches this via the matrix
	// tunnel).
	LocalAddr string

	// Identity displayed in the cloud portal's host list.
	AgentName string

	// Matrix tunnel connection to cloudbox.
	ServerAddr string
	ServerPort int
	// Protocol is the matrix-tunnel transport ("tcp", "ws", or "wss").
	// When unset the agent uses raw TCP (legacy default). The register
	// exchange usually fills this in based on how cloudbox is fronted.
	Protocol string
	Token    string

	// RemotePort the agent asks cloudbox to reserve for its local HTTP
	// server (so the cloud's HostRegistry can dial it). 0 means
	// auto-assign.
	RemotePort int

	// Apps registered with this agent. Comma-separated "name=url" pairs,
	// e.g. "ycode=http://127.0.0.1:8765,pihole=http://192.168.1.5/admin".
	Apps string

	// AdminUsers is the optional comma-separated list of OAuth-identified
	// emails that should NOT be auto-promoted to admin when authenticating
	// via the host OS path. Default behavior is admin-on-OS-success
	// (anyone who can prove the OS password owns the box). Setting this
	// list switches to a strict "only these emails are admin" mode for the
	// OS-auth branch. The AuthURL branch ignores this entirely — the
	// external auth endpoint owns role assignment.
	AdminUsers string

	// AuthURL, when non-empty, makes the agent's /auth handler delegate
	// credential verification to an external HTTP endpoint instead of the
	// host OS. The endpoint receives {user, password} and is expected to
	// return {user, role}. Application-level users live behind this URL.
	AuthURL string
}

func Load() (*Config, error) {
	cfg := &Config{
		LocalAddr:  getenv("AGENT_ADDR", "127.0.0.1:0"),
		AgentName:  os.Getenv("AGENT_NAME"),
		ServerAddr: getenv("MATRIX_SERVER_ADDR", "127.0.0.1"),
		Token:      os.Getenv("MATRIX_TOKEN"),
	}

	port, err := strconv.Atoi(getenv("MATRIX_SERVER_PORT", "7000"))
	if err != nil {
		return nil, fmt.Errorf("MATRIX_SERVER_PORT: %w", err)
	}
	cfg.ServerPort = port

	remote, err := strconv.Atoi(getenv("MATRIX_REMOTE_PORT", "0"))
	if err != nil {
		return nil, fmt.Errorf("MATRIX_REMOTE_PORT: %w", err)
	}
	cfg.RemotePort = remote

	cfg.Apps = os.Getenv("MATRIX_APPS")
	cfg.AdminUsers = os.Getenv("MATRIX_ADMIN_USERS")
	cfg.AuthURL = os.Getenv("MATRIX_AUTH_URL")
	cfg.Protocol = os.Getenv("MATRIX_PROTOCOL")

	return cfg, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
