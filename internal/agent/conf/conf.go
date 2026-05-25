// Package conf holds the matrix-agent runtime configuration.
package conf

import (
	"fmt"
	"os"
	"strconv"
)

// Config drives the matrix-agent binary. Layering precedence in
// `outpost start` is: CLI flag > env var > FileConfig value > hardcoded
// default. Load() returns only what the environment supplies — empty
// strings / zero ints mean "use the FileConfig value if any, else fall
// back to a default at the call site". Don't bake defaults into Load();
// that defeats file-based overrides.
type Config struct {
	// Local loopback HTTP server (cloudbox reaches this via the matrix
	// tunnel).
	LocalAddr string

	// VNCAddr is the upstream the built-in /desktop route bridges to.
	// Empty here means "fall through to FileConfig.VNCAddr or the
	// hardcoded 127.0.0.1:5900 default at the call site".
	VNCAddr string

	// AdminAddr is the bind address for the admin UI + MCP loopback
	// listener. Same fall-through semantics as VNCAddr.
	AdminAddr string

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

	// AdminUsers is the optional allowlist of OAuth-identified emails
	// that should be treated as admin when authenticating via the host
	// OS path. Comma-separated when sourced from env (`MATRIX_ADMIN_USERS`).
	// Default behavior is admin-on-OS-success (anyone who can prove the
	// OS password owns the box). Setting this list switches to a strict
	// "only these emails are admin" mode for the OS-auth branch. The
	// AuthURL branch ignores this entirely — the external auth endpoint
	// owns role assignment.
	AdminUsers string

	// AuthURL, when non-empty, makes the agent's /auth handler delegate
	// credential verification to an external HTTP endpoint instead of the
	// host OS. The endpoint receives {user, password} and is expected to
	// return {user, role}. Application-level users live behind this URL.
	AuthURL string
}

// Load reads env vars into a Config. Returns empty strings / zero ints
// when env is absent — callers layer FileConfig values and hardcoded
// defaults afterwards. This is deliberate: baking defaults into Load
// would make file-based overrides impossible (the env-supplied default
// always wins over an empty file field).
func Load() (*Config, error) {
	cfg := &Config{
		LocalAddr:  os.Getenv("AGENT_ADDR"),
		VNCAddr:    os.Getenv("AGENT_VNC_ADDR"),
		AdminAddr:  os.Getenv("OUTPOST_ADMIN_ADDR"),
		AgentName:  os.Getenv("AGENT_NAME"),
		ServerAddr: os.Getenv("MATRIX_SERVER_ADDR"),
		Token:      os.Getenv("MATRIX_TOKEN"),
	}

	if v := os.Getenv("MATRIX_SERVER_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MATRIX_SERVER_PORT: %w", err)
		}
		cfg.ServerPort = port
	}

	if v := os.Getenv("MATRIX_REMOTE_PORT"); v != "" {
		remote, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MATRIX_REMOTE_PORT: %w", err)
		}
		cfg.RemotePort = remote
	}

	cfg.Apps = os.Getenv("MATRIX_APPS")
	cfg.AdminUsers = os.Getenv("MATRIX_ADMIN_USERS")
	cfg.AuthURL = os.Getenv("MATRIX_AUTH_URL")
	cfg.Protocol = os.Getenv("MATRIX_PROTOCOL")

	return cfg, nil
}

// Default values applied at the call site when neither env nor file
// supplied anything. Centralized so the CLI help text, the admin UI
// placeholder, and the boot code all agree on the same values.
const (
	DefaultLocalAddr  = "127.0.0.1:0"
	DefaultVNCAddr    = "127.0.0.1:5900"
	DefaultAdminAddr  = "127.0.0.1:17777"
	DefaultServerAddr = "127.0.0.1"
	DefaultServerPort = 7000
)
