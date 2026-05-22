package conf

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileConfig is what the register command writes and `start` reads from
// disk. It pins everything the agent needs to dial the cloud — no more
// env juggling once registration has completed.
//
// AuthURL, when non-empty, switches the agent's /auth handler from the
// host OS (PAM / dscl / LogonUserW) to an external HTTP endpoint that
// owns its own application-level user list.
type FileConfig struct {
	AgentName  string `json:"agent_name"`
	ServerAddr string `json:"server_addr"`
	ServerPort int    `json:"server_port"`
	// Protocol is "tcp" (default for legacy raw-TCP matrix-tunnel
	// deploys), "ws", or "wss". Returned by /api/register/exchange so
	// the outpost knows how cloudbox expects to be dialed. Empty == "tcp".
	Protocol   string `json:"protocol,omitempty"`
	Token      string `json:"token"`
	RemotePort int    `json:"remote_port"`
	AuthURL    string `json:"auth_url,omitempty"`

	// AccessToken is the per-outpost scoped JWT cloudbox issues at
	// register time. Bearer-auth credential for /h/:host/ssh (used by
	// `outpost ssh-proxy`) and /api/v1/ssh/* (used by `outpost
	// ssh-config`). Distinct from Token, which is the *matrix-tunnel*
	// shared secret used by the FRP client.
	AccessToken string `json:"access_token,omitempty"`

	// ClientOnly marks this machine as a credential vehicle that should
	// never accept inbound traffic — the user wants to ssh OUT to other
	// paired hosts but not BE one. When true: `outpost start` skips
	// NewTunnel + the local gin server, /apps/etc. don't bind, and the
	// admin UI is the only loopback listener (for managing this row).
	ClientOnly bool `json:"client_only,omitempty"`

	// Apps managed through the admin UI. When this field is present (even
	// empty), it is authoritative — the legacy MATRIX_APPS env is ignored.
	// When absent (nil) on a config written before the admin UI shipped,
	// `start` falls back to MATRIX_APPS for back-compat.
	Apps []AppConfig `json:"apps,omitempty"`

	// Built-in route toggles. Pointer-bool so a missing field on an old
	// config means "default on", which matches the pre-admin-UI behavior.
	// Use ShellOn()/DesktopOn()/ClipboardOn()/SSHOn() to read; never deref directly.
	ShellEnabled     *bool `json:"shell_enabled,omitempty"`
	DesktopEnabled   *bool `json:"desktop_enabled,omitempty"`
	ClipboardEnabled *bool `json:"clipboard_enabled,omitempty"`
	SSHEnabled       *bool `json:"ssh_enabled,omitempty"`

	// Built-in proxies for local daemons. Default off (plain bool) — these
	// expose external infrastructure rather than outpost-owned routes, so
	// they require explicit opt-in via the admin UI. The UI greys these
	// toggles out when the daemon isn't actually running on this host.
	PodmanEnabled bool `json:"podman_enabled,omitempty"`
	OllamaEnabled bool `json:"ollama_enabled,omitempty"`

	// AdminSessionKey is the HMAC secret used to sign admin-UI session
	// cookies. Persisting it across restarts is what keeps the admin user
	// logged in when a built-in toggle re-execs the binary. Base64-encoded
	// in the JSON (32 random bytes worth of entropy). Auto-generated and
	// saved on first boot via EnsureAdminSessionKey.
	AdminSessionKey []byte `json:"admin_session_key,omitempty"`

	// Outbound configures local mount paths that proxy through cloudbox to
	// remote outposts' apps. The local outpost holds an in-memory
	// elevation cookie per entry (captured by Connect); after that, the
	// local URL http://localhost:17777/<path>/ proxies to
	// https://<cloudbox>/h/<host>/app/<name>/<rest>. See
	// internal/agent/outbound.go.
	Outbound []OutboundConfig `json:"outbound,omitempty"`
}

// OutboundConfig is one local mount that proxies to a remote outpost.
//
//   - Path : local mount under the admin UI listener — e.g. "kg" makes the
//     remote app reachable at http://localhost:17777/kg/.
//   - Name : the remote outpost's app name (e.g. "ollama"). Matched
//     against the remote's AppRegistry by the cloudbox host-proxy.
//   - Host : the remote outpost's name as registered with cloudbox.
//   - User : the OS user on the remote outpost (used at Connect time
//     when POSTing to /h/<host>/elevate).
type OutboundConfig struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Host string `json:"host"`
	User string `json:"user"`
}

// AppConfig is one custom reverse-proxy target. It is mounted under
// /app/<name>/ on the agent and the cloud reaches it through the tunnel.
//
// Scheme picks the transport:
//   - "http" / "https": classic TCP target. Use Host (default 127.0.0.1)
//     and Port. Socket is ignored.
//   - "unix": AF_UNIX socket at Socket. Works on Linux, macOS, and
//     Windows (AF_UNIX since Win10 1803). Host/Port are ignored.
//   - "npipe": Windows named pipe at Socket (e.g. \\.\pipe\docker_engine).
//     Only supported on Windows builds; non-Windows builds reject it at
//     request time. Host/Port are ignored.
type AppConfig struct {
	Name    string `json:"name"`
	Icon    string `json:"icon,omitempty"`
	Scheme  string `json:"scheme"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Socket  string `json:"socket,omitempty"`
	Enabled bool   `json:"enabled"`
	// Role is the minimum cloud-side clearance required to reach this app
	// (guest|user|admin). Empty defaults to "user" — matches the cloud's
	// HostRegistry default so unconfigured apps keep working unchanged.
	Role string `json:"role,omitempty"`
}

// IsSocket reports whether ac targets a local socket (unix or npipe)
// rather than a TCP host:port.
func (ac AppConfig) IsSocket() bool {
	s := strings.ToLower(strings.TrimSpace(ac.Scheme))
	return s == "unix" || s == "npipe"
}

// AppTargetFromURL parses a single URL string ("http://localhost:8080",
// "unix:///run/podman/podman.sock", etc.) into the scheme/host/port/
// socket fields that AppConfig stores. The admin UI sends URLs; the
// server splits them here so the persisted record stays in the same
// shape that older configs and the AppRegistry already understand.
//
// http/https URLs use the default port when none is given (80/443).
// unix URLs may use either a `unix:///abs/path` or `unix:/abs/path`
// form; both are accepted. Returns an error on anything else.
func AppTargetFromURL(raw string) (scheme, host string, port int, socket string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0, "", fmt.Errorf("url is required")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", 0, "", fmt.Errorf("parse url: %w", perr)
	}
	scheme = strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
		host = u.Hostname()
		if host == "" {
			return "", "", 0, "", fmt.Errorf("url %q is missing host", raw)
		}
		if p := u.Port(); p != "" {
			n, cerr := strconv.Atoi(p)
			if cerr != nil || n < 1 || n > 65535 {
				return "", "", 0, "", fmt.Errorf("url %q has invalid port", raw)
			}
			port = n
		} else if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
		return scheme, host, port, "", nil
	case "unix", "npipe":
		// `unix:///path` → u.Path = "/path"; `unix:/path` → also "/path".
		// `unix://host/path` is technically valid but we treat the host
		// segment as advisory and use the path.
		sock := u.Path
		if sock == "" {
			sock = u.Opaque
		}
		if sock == "" {
			return "", "", 0, "", fmt.Errorf("url %q is missing socket path", raw)
		}
		return scheme, "", 0, sock, nil
	default:
		return "", "", 0, "", fmt.Errorf("url %q: scheme must be one of http|https|unix|npipe", raw)
	}
}

// ValidRole reports whether s is a recognized clearance level.
func ValidRole(s string) bool {
	switch s {
	case "", "guest", "user", "admin":
		return true
	}
	return false
}

// ShellOn reports whether the built-in /shell route should be mounted.
// Missing field (old configs) defaults to true.
func (fc *FileConfig) ShellOn() bool { return fc == nil || fc.ShellEnabled == nil || *fc.ShellEnabled }

// DesktopOn reports whether the built-in /desktop route should be mounted.
func (fc *FileConfig) DesktopOn() bool {
	return fc == nil || fc.DesktopEnabled == nil || *fc.DesktopEnabled
}

// ClipboardOn reports whether the built-in /clipboard route should be mounted.
func (fc *FileConfig) ClipboardOn() bool {
	return fc == nil || fc.ClipboardEnabled == nil || *fc.ClipboardEnabled
}

// SSHOn reports whether the built-in /ssh route (real SSH server reached
// over WebSocket through the matrix tunnel) should be mounted.
func (fc *FileConfig) SSHOn() bool { return fc == nil || fc.SSHEnabled == nil || *fc.SSHEnabled }

// PodmanOn reports whether the built-in podman proxy is enabled in this
// config. Unlike the loopback-only builtins above, podman is off by
// default — the admin UI flips it on after the daemon is detected.
func (fc *FileConfig) PodmanOn() bool { return fc != nil && fc.PodmanEnabled }

// OllamaOn reports whether the built-in Ollama proxy is enabled.
func (fc *FileConfig) OllamaOn() bool { return fc != nil && fc.OllamaEnabled }

// DefaultConfigPath is ~/.config/matrix/agent.json (XDG_CONFIG_HOME honored).
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "matrix", "agent.json"), nil
}

// SaveFile writes fc atomically (write+rename) to path, creating parents.
func SaveFile(path string, fc *FileConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(fc); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EnsureAdminSessionKey returns fc.AdminSessionKey, generating a fresh
// 32-byte random key (and persisting it via SaveFile at path) if the
// field is empty. Callers MUST pass a non-nil fc that they've already
// loaded (or freshly constructed). The returned slice points at the
// same backing array as fc.AdminSessionKey.
//
// Why this lives here: the key has to outlive the process, so it
// belongs in the on-disk FileConfig; but it's the admin UI server that
// uses it. Centralizing the load-or-create here lets main.go thread it
// into adminui.Deps without duplicating the IO dance.
func EnsureAdminSessionKey(path string, fc *FileConfig) ([]byte, error) {
	if fc == nil {
		return nil, fmt.Errorf("nil FileConfig")
	}
	if len(fc.AdminSessionKey) >= 16 {
		return fc.AdminSessionKey, nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("generate admin session key: %w", err)
	}
	fc.AdminSessionKey = b[:]
	if path != "" {
		if err := SaveFile(path, fc); err != nil {
			return nil, fmt.Errorf("save admin session key: %w", err)
		}
	}
	return fc.AdminSessionKey, nil
}

// LoadFile reads a previously-saved FileConfig. Returns (nil, nil) if the
// file doesn't exist — callers should fall back to env.
func LoadFile(path string) (*FileConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var fc FileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &fc, nil
}
