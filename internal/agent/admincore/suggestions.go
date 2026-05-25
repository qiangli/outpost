package admincore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Suggestion is one auto-detected local app the operator can register
// with a single click (in the admin UI) or a single MCP call.
type Suggestion struct {
	Name     string `json:"name"`
	Scheme   string `json:"scheme"`
	Socket   string `json:"socket,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Role     string `json:"role"`
	Source   string `json:"source"`             // "wellKnown" | "ycodeManifest"
	Note     string `json:"note,omitempty"`     // human-readable hint
	Existing bool   `json:"existing,omitempty"` // already registered with this name
}

// AppSuggestions probes well-known socket paths and the local ycode
// manifest, returning the apps the user could enable with one click.
// Never mutates configuration.
func (s *Server) AppSuggestions() ([]Suggestion, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	registered := map[string]bool{}
	for _, a := range fc.Apps {
		registered[a.Name] = true
	}

	out := []Suggestion{}
	for _, sug := range wellKnownPodmanCandidates() {
		if _, err := os.Stat(sug.Socket); err != nil {
			continue
		}
		sug.Existing = registered[sug.Name]
		out = append(out, sug)
	}
	if sug := ycodeOllamaSuggestion(); sug != nil {
		sug.Existing = registered[sug.Name]
		out = append(out, *sug)
	}
	if sug := ycodeGatewaySuggestion(); sug != nil {
		sug.Existing = registered[sug.Name]
		out = append(out, *sug)
	}
	return out, nil
}

// wellKnownPodmanCandidates returns the per-OS list of paths where
// podman / docker sockets typically live. Returned entries are not
// filtered by existence; the caller does the os.Stat.
func wellKnownPodmanCandidates() []Suggestion {
	uid := strconv.Itoa(os.Getuid())
	switch runtime.GOOS {
	case "linux":
		return []Suggestion{
			{
				Name: "podman", Scheme: "unix", Role: "admin", Source: "wellKnown",
				Socket: "/run/user/" + uid + "/podman/podman.sock",
				Note:   "rootless podman (per-user socket)",
			},
			{
				Name: "podman-rootful", Scheme: "unix", Role: "admin", Source: "wellKnown",
				Socket: "/run/podman/podman.sock",
				Note:   "rootful podman (system socket)",
			},
			{
				Name: "docker", Scheme: "unix", Role: "admin", Source: "wellKnown",
				Socket: "/var/run/docker.sock",
				Note:   "docker daemon",
			},
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		return []Suggestion{
			{
				Name: "podman", Scheme: "unix", Role: "admin", Source: "wellKnown",
				Socket: filepath.Join(home, ".local/share/containers/podman/machine/podman.sock"),
				Note:   "podman machine (default)",
			},
			{
				Name: "docker", Scheme: "unix", Role: "admin", Source: "wellKnown",
				Socket: filepath.Join(home, ".docker/run/docker.sock"),
				Note:   "Docker Desktop",
			},
		}
	case "windows":
		// On Windows the engine speaks named pipes, not AF_UNIX.
		// os.Stat won't see these the same way; we surface them
		// unconditionally and let the user pick.
		return []Suggestion{
			{
				Name: "podman", Scheme: "npipe", Role: "admin", Source: "wellKnown",
				Socket: `\\.\pipe\podman-machine-default`,
				Note:   "podman machine on Windows",
			},
			{
				Name: "docker", Scheme: "npipe", Role: "admin", Source: "wellKnown",
				Socket: `\\.\pipe\docker_engine`,
				Note:   "Docker Desktop on Windows",
			},
		}
	}
	return nil
}

// ycodeOllamaSuggestion returns the well-known local ollama HTTP
// suggestion. We don't dial the port here (would tie request latency
// to the daemon's responsiveness); we just list it as a candidate.
func ycodeOllamaSuggestion() *Suggestion {
	return &Suggestion{
		Name:   "ollama",
		Scheme: "http",
		Host:   "127.0.0.1",
		Port:   11434,
		Role:   "user",
		Source: "wellKnown",
		Note:   "Ollama HTTP API (default port)",
	}
}

// ycodeGatewaySuggestion reads ~/.agents/ycode/manifest.json and, if it
// publishes a gateway block in embedded mode, returns a suggestion to
// register that endpoint as an outpost app.
func ycodeGatewaySuggestion() *Suggestion {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	manifestPath := filepath.Join(home, ".agents", "ycode", "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var m struct {
		Gateway struct {
			Podman struct {
				Socket string `json:"socket"`
				Mode   string `json:"mode"`
			} `json:"podman"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if m.Gateway.Podman.Socket == "" {
		return nil
	}
	// Only suggest the ycode gateway when it's running in EMBEDDED
	// mode. In remote mode the gateway itself is just a forwarder —
	// exposing it through outpost would loop back to the same
	// cloudbox URL.
	if m.Gateway.Podman.Mode != "embedded" {
		return nil
	}
	return &Suggestion{
		Name:   "ycode-podman",
		Scheme: "unix",
		Socket: m.Gateway.Podman.Socket,
		Role:   "admin",
		Source: "ycodeManifest",
		Note:   "ycode gateway (embedded mode) — exposes this host's podman to remote ycode clients",
	}
}

// OutboundSuggestion is one row in the "Remote" dropdown — a host + an
// app on that host (or a synthetic SSH row pointing at the host's
// built-in /ssh endpoint).
type OutboundSuggestion struct {
	Host         string `json:"host"`
	OsUser       string `json:"os_user,omitempty"`
	Name         string `json:"name"`
	Scheme       string `json:"scheme,omitempty"`
	RequireLogin bool   `json:"require_login"`
	IndexPath    string `json:"index_path,omitempty"`
	Title        string `json:"title,omitempty"`
	Online       bool   `json:"online"`
	Shared       bool   `json:"shared,omitempty"`
}

// OutboundSuggestions calls cloudbox's /api/v1/hosts and flattens it
// into one row per (host, app), plus a synthetic SSH row per host whose
// built-in /ssh is mounted. Returns ServiceUnavailable when the outpost
// isn't paired yet (no AccessToken to authenticate with).
func (s *Server) OutboundSuggestions(ctx context.Context) ([]OutboundSuggestion, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	if fc == nil || fc.AccessToken == "" {
		return nil, unavailable("outpost has no access_token — pair with cloudbox first")
	}
	rows, err := fetchHostsFromCloudbox(ctx, fc.ServerAddr, fc.ServerPort, fc.Protocol, fc.AccessToken)
	if err != nil {
		return nil, upstream("%s", err.Error())
	}
	out := []OutboundSuggestion{}
	for _, h := range rows {
		for _, a := range h.Apps {
			scheme := a.Scheme
			if scheme == "" {
				scheme = "http" // legacy outposts that don't ship the field
			}
			out = append(out, OutboundSuggestion{
				Host:         h.Host,
				OsUser:       h.OsUser,
				Name:         a.Name,
				Scheme:       scheme,
				RequireLogin: a.RequireLogin,
				IndexPath:    a.IndexPath,
				Title:        h.Title,
				Online:       h.Online,
				Shared:       h.Shared,
			})
		}
		// Synthetic suggestion for the host's built-in /ssh server
		// when the remote outpost actually has /ssh mounted. Older
		// outposts omit `builtins`, which we treat as "all on" for
		// backward compat.
		if h.Builtins == nil || h.Builtins.SSH == nil || *h.Builtins.SSH {
			out = append(out, OutboundSuggestion{
				Host:   h.Host,
				OsUser: h.OsUser,
				Name:   "", // signals built-in /ssh to the SPA
				Scheme: "ssh",
				Title:  h.Title,
				Online: h.Online,
				Shared: h.Shared,
			})
		}
	}
	return out, nil
}

// cbHostEntry mirrors the cloudbox /api/v1/hosts response. Defined
// locally so admincore stays decoupled from any internal cloudbox
// types — the field set is intentionally narrow.
type cbHostEntry struct {
	Host     string       `json:"host"`
	OsUser   string       `json:"os_user"`
	Title    string       `json:"title"`
	Online   bool         `json:"online"`
	Shared   bool         `json:"shared"`
	Apps     []cbAppEntry `json:"apps"`
	Builtins *cbBuiltins  `json:"builtins,omitempty"`
}

type cbAppEntry struct {
	Name         string `json:"name"`
	Scheme       string `json:"scheme"`
	RequireLogin bool   `json:"require_login"`
	IndexPath    string `json:"index_path"`
}

// cbBuiltins mirrors the `builtins` map cloudbox exposes per host.
// Pointer fields distinguish "absent (legacy)" from "explicitly false".
type cbBuiltins struct {
	Shell     *bool `json:"shell,omitempty"`
	Desktop   *bool `json:"desktop,omitempty"`
	Clipboard *bool `json:"clipboard,omitempty"`
	SSH       *bool `json:"ssh,omitempty"`
}

// fetchHostsFromCloudbox calls /api/v1/hosts on the configured cloudbox
// with the outpost's persisted access_token. Protocol/port follow the
// same wss↔https / ws↔http mapping ssh-proxy uses.
func fetchHostsFromCloudbox(ctx context.Context, server string, port int, protocol, token string) ([]cbHostEntry, error) {
	srv := strings.TrimSpace(server)
	if srv == "" {
		return nil, fmt.Errorf("cloudbox URL not configured")
	}
	if !strings.Contains(srv, "://") {
		srv = "http://" + srv
	}
	u, err := url.Parse(srv)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(protocol), "wss") || strings.EqualFold(u.Scheme, "https") {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if u.Port() == "" && port > 0 {
		u.Host = u.Hostname() + ":" + strconv.Itoa(port)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/hosts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("cloudbox /api/v1/hosts %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Hosts []cbHostEntry `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /api/v1/hosts: %w", err)
	}
	return out.Hosts, nil
}
