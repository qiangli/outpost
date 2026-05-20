package adminui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Suggestion is one auto-detected app the user can register with a
// single click in the admin UI.
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

// handleListSuggestions probes well-known socket paths and the local
// ycode manifest, returning the apps the user could enable with one
// click. It never mutates configuration — the user still has to POST
// /api/apps to actually register.
func (s *Server) handleListSuggestions(c *gin.Context) {
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	registered := map[string]bool{}
	for _, a := range fc.Apps {
		registered[a.Name] = true
	}

	out := []Suggestion{}
	for _, s := range wellKnownPodmanCandidates() {
		if _, err := os.Stat(s.Socket); err != nil {
			continue
		}
		s.Existing = registered[s.Name]
		out = append(out, s)
	}
	if s := ycodeOllamaSuggestion(); s != nil {
		s.Existing = registered[s.Name]
		out = append(out, *s)
	}
	if s := ycodeGatewaySuggestion(); s != nil {
		s.Existing = registered[s.Name]
		out = append(out, *s)
	}
	c.JSON(http.StatusOK, gin.H{"suggestions": out})
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
// suggestion when the port is reachable. We don't dial the port here
// (would tie request latency to the daemon's responsiveness); we just
// list it as a candidate.
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
// publishes a gateway block, returns suggestions to register those
// endpoints as outpost apps. This lets a remote ycode reach the home
// ycode's gateway through outpost without manually copying paths.
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
	// Only suggest the ycode gateway when it's running in EMBEDDED mode.
	// In remote mode the gateway itself is just a forwarder — exposing
	// it through outpost would loop back to the same cloudbox URL.
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

// ensureSuggestionConsistent is a sanity check used in tests: a socket-
// backed Suggestion must declare a Socket; an http one must declare a Port.
func (s Suggestion) valid() bool {
	if conf.AppConfig(s.toAppConfig()).IsSocket() {
		return s.Socket != ""
	}
	return s.Port > 0
}

// toAppConfig is a one-shot conversion to feed into validator/registry
// paths that want a conf.AppConfig (used by tests).
func (s Suggestion) toAppConfig() conf.AppConfig {
	return conf.AppConfig{
		Name:    s.Name,
		Scheme:  s.Scheme,
		Host:    s.Host,
		Port:    s.Port,
		Socket:  s.Socket,
		Enabled: true,
		Role:    s.Role,
	}
}
