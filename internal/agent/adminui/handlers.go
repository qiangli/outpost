package adminui

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/portal"
)

// loadConfig reads the FileConfig (or returns an empty one on first run).
// Callers must hold s.mu when intending to write back.
func (s *Server) loadConfig() (*conf.FileConfig, error) {
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		return nil, err
	}
	if fc == nil {
		fc = &conf.FileConfig{}
	}
	return fc, nil
}

// builtinView is the admin-UI shape for one optional local-daemon proxy
// (podman/ollama). Enabled reflects the saved config; Available is the
// live detection result so the UI can grey out the toggle when the
// daemon isn't running. Target is a human-readable description of where
// the proxy would point ("unix:///run/podman/...", "http://127.0.0.1:11434").
type builtinView struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Target    string `json:"target,omitempty"`
}

func toBuiltinView(enabled bool, bt agent.BuiltinTarget) builtinView {
	v := builtinView{Enabled: enabled, Available: bt.Available}
	switch bt.Scheme {
	case "unix", "npipe":
		if bt.Socket != "" {
			v.Target = bt.Scheme + "://" + bt.Socket
		}
	case "http", "https":
		v.Target = bt.URL
	}
	return v
}

// safeView is the redacted FileConfig sent over the API. Token never
// leaves the agent; presence is reported as has_token instead.
type safeView struct {
	AgentName        string               `json:"agent_name"`
	ServerAddr       string               `json:"server_addr"`
	ServerPort       int                  `json:"server_port"`
	Protocol         string               `json:"protocol,omitempty"`
	RemotePort       int                  `json:"remote_port"`
	AuthURL          string               `json:"auth_url,omitempty"`
	HasToken         bool                 `json:"has_token"`
	Apps             []conf.AppConfig     `json:"apps"`
	ShellEnabled     bool                 `json:"shell_enabled"`
	DesktopEnabled   bool                 `json:"desktop_enabled"`
	ClipboardEnabled bool                 `json:"clipboard_enabled"`
	SSHEnabled       bool                 `json:"ssh_enabled"`
	Podman           builtinView          `json:"podman"`
	Ollama           builtinView          `json:"ollama"`
	Outbound         []agent.OutboundView `json:"outbound"`
	Defaults         map[string]string    `json:"defaults"`
}

func (s *Server) toSafeView(fc *conf.FileConfig) safeView {
	apps := fc.Apps
	if apps == nil {
		apps = []conf.AppConfig{}
	}
	osUser, _ := hostauth.CurrentUser()
	osHost, _ := os.Hostname()
	defaultName := osHost
	if osHost != "" && osUser != "" {
		defaultName = osHost + "-" + osUser
	}
	return safeView{
		AgentName:        fc.AgentName,
		ServerAddr:       fc.ServerAddr,
		ServerPort:       fc.ServerPort,
		Protocol:         fc.Protocol,
		RemotePort:       fc.RemotePort,
		AuthURL:          fc.AuthURL,
		HasToken:         fc.Token != "",
		Apps:             apps,
		ShellEnabled:     fc.ShellOn(),
		DesktopEnabled:   fc.DesktopOn(),
		ClipboardEnabled: fc.ClipboardOn(),
		SSHEnabled:       fc.SSHOn(),
		Podman:           toBuiltinView(fc.PodmanOn(), s.detector.Podman()),
		Ollama:           toBuiltinView(fc.OllamaOn(), s.detector.Ollama()),
		Outbound:         s.outboundList(),
		Defaults: map[string]string{
			"server_url": "https://ai.dhnt.io",
			"name":       defaultName,
			"os_user":    osUser,
		},
	}
}

// handleStatus is the SPA's "what should I render?" call.
func (s *Server) handleStatus(c *gin.Context) {
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	osUser, _ := hostauth.CurrentUser()
	c.JSON(http.StatusOK, gin.H{
		"configured":      fc.AgentName != "",
		"agent_name":      fc.AgentName,
		"server_addr":     fc.ServerAddr,
		"current_os_user": osUser,
	})
}

type loginReq struct {
	User     string `json:"user"`
	Password string `json:"password" binding:"required"`
}

// handleLogin verifies the OS password and mints a session cookie. The
// submitted username MUST equal the running OS user — same gate as the
// main /auth handler for consistency.
func (s *Server) handleLogin(c *gin.Context) {
	if s.loginRL != nil && !s.loginRL.Allow(c.ClientIP()) {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts"})
		return
	}
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	current, _ := hostauth.CurrentUser()
	if current == "" {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "cannot determine current OS user"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.User), current) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if err := s.deps.Auth.Authenticate(current, req.Password); err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	cookie, err := s.sessions.Mint(current)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// MaxAge matches the session TTL. Secure=false because admin UI is
	// HTTP loopback; that's safe by design.
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(cookieName, cookie, int(time.Hour.Seconds()), "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"user": current})
}

func (s *Server) handleLogout(c *gin.Context) {
	if cookie, err := c.Cookie(cookieName); err == nil {
		s.sessions.Revoke(cookie)
	}
	c.SetCookie(cookieName, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleGetConfig(c *gin.Context) {
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s.toSafeView(fc))
}

type registerReq struct {
	Server  string `json:"server"`
	Code    string `json:"code"`
	Name    string `json:"name"`
	Title   string `json:"title"`
	AuthURL string `json:"auth_url"`
}

// handleRegister runs the pairing exchange and saves the resulting config
// (preserving any apps/toggles that were already on disk). Then schedules
// a restart so the new tunnel/identity takes effect.
func (s *Server) handleRegister(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Code) == "" || strings.TrimSpace(req.Name) == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "server, code, and name are required"})
		return
	}
	server := strings.TrimSpace(req.Server)
	if server == "" {
		server = "https://ai.dhnt.io"
	}

	exchanged, err := portal.Exchange(c.Request.Context(), portal.ExchangeRequest{
		ServerURL: server,
		Code:      req.Code,
		Name:      req.Name,
		Title:     req.Title,
		AuthURL:   req.AuthURL,
	})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Merge: keep locally-managed fields (apps, toggles) and overwrite
	// portal-controlled fields with the exchanged values.
	merged := *existing
	merged.AgentName = exchanged.AgentName
	merged.ServerAddr = exchanged.ServerAddr
	merged.ServerPort = exchanged.ServerPort
	merged.Protocol = exchanged.Protocol
	merged.Token = exchanged.Token
	merged.RemotePort = exchanged.RemotePort
	merged.AuthURL = exchanged.AuthURL
	merged.AccessToken = exchanged.AccessToken

	if err := conf.SaveFile(s.deps.ConfigPath, &merged); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "restarting": true, "agent_name": merged.AgentName})
	s.scheduleRestart()
}

type builtinsReq struct {
	Shell     *bool `json:"shell"`
	Desktop   *bool `json:"desktop"`
	Clipboard *bool `json:"clipboard"`
	SSH       *bool `json:"ssh"`
	Podman    *bool `json:"podman"`
	Ollama    *bool `json:"ollama"`
}

// handleBuiltins toggles built-in routes (shell/desktop/clipboard/ssh)
// and the optional local-daemon proxies (podman/ollama). All of these
// are wired at boot, so saving triggers a restart.
//
// Enabling podman/ollama when the daemon isn't detected is allowed —
// the user might be about to start the daemon. The boot path simply
// skips the registration when the probe still fails, and the toggle
// stays "on but inactive" until the daemon shows up.
func (s *Server) handleBuiltins(c *gin.Context) {
	var req builtinsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if req.Shell != nil {
		fc.ShellEnabled = req.Shell
	}
	if req.Desktop != nil {
		fc.DesktopEnabled = req.Desktop
	}
	if req.Clipboard != nil {
		fc.ClipboardEnabled = req.Clipboard
	}
	if req.SSH != nil {
		fc.SSHEnabled = req.SSH
	}
	if req.Podman != nil {
		fc.PodmanEnabled = *req.Podman
	}
	if req.Ollama != nil {
		fc.OllamaEnabled = *req.Ollama
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "restarting": fc.AgentName != ""})
	if fc.AgentName != "" {
		// Only restart when the tunnel was running. On first-time setup
		// nothing is mounted yet, so a save is harmless.
		s.scheduleRestart()
	}
}

func (s *Server) handleListApps(c *gin.Context) {
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	apps := fc.Apps
	if apps == nil {
		apps = []conf.AppConfig{}
	}
	c.JSON(http.StatusOK, gin.H{"apps": apps})
}

// upsertAppReq accepts both the legacy {scheme, host, port, socket}
// shape and the new single-URL form ("http://localhost:8080",
// "unix:///run/podman/podman.sock"). When URL is set, it wins — the
// split fields are derived from it.
type upsertAppReq struct {
	conf.AppConfig
	URL string `json:"url,omitempty"`
}

// handleUpsertApp validates one AppConfig, persists it, and mutates the
// live registry. No restart required — AppRegistry is concurrency-safe.
func (s *Server) handleUpsertApp(c *gin.Context) {
	var req upsertAppReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ac := req.AppConfig
	if strings.TrimSpace(req.URL) != "" {
		scheme, host, port, socket, perr := conf.AppTargetFromURL(req.URL)
		if perr != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": perr.Error()})
			return
		}
		ac.Scheme, ac.Host, ac.Port, ac.Socket = scheme, host, port, socket
	}
	if err := validateApp(&ac); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Upsert: replace any existing entry with the same name, else append.
	if fc.Apps == nil {
		fc.Apps = []conf.AppConfig{}
	}
	replaced := false
	for i, existing := range fc.Apps {
		if existing.Name == ac.Name {
			fc.Apps[i] = ac
			replaced = true
			break
		}
	}
	if !replaced {
		fc.Apps = append(fc.Apps, ac)
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Reflect into the live registry. Unregister first to handle the
	// edit case (target URL changed) and the disable case (enabled=false).
	s.deps.Apps.Unregister(ac.Name)
	if ac.Enabled {
		if err := s.deps.Apps.RegisterFromConfig(ac); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "app": ac})
}

func (s *Server) handleDeleteApp(c *gin.Context) {
	name := c.Param("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	filtered := fc.Apps[:0]
	for _, app := range fc.Apps {
		if app.Name != name {
			filtered = append(filtered, app)
		}
	}
	fc.Apps = filtered
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.deps.Apps.Unregister(name)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleRestart(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"ok": true, "restarting": true})
	s.scheduleRestart()
}

func validateApp(ac *conf.AppConfig) error {
	ac.Name = strings.TrimSpace(ac.Name)
	if ac.Name == "" {
		return errors.New("name is required")
	}
	if strings.ContainsAny(ac.Name, "/ \t") {
		return errors.New("name cannot contain slashes or whitespace")
	}
	// Reserved by the admin UI's own routes. Allowing an app with one of
	// these names would shadow the admin API or the local-proxy itself.
	switch strings.ToLower(ac.Name) {
	case "api", "static", "healthz", "index.html", "app":
		return fmt.Errorf("name %q is reserved by the admin UI", ac.Name)
	}
	ac.Scheme = strings.ToLower(strings.TrimSpace(ac.Scheme))
	if ac.Scheme == "" {
		ac.Scheme = "http"
	}
	switch ac.Scheme {
	case "http", "https":
		ac.Host = strings.TrimSpace(ac.Host)
		if ac.Host == "" {
			ac.Host = "127.0.0.1"
		}
		if ac.Port < 1 || ac.Port > 65535 {
			return fmt.Errorf("port %d is out of range", ac.Port)
		}
		// Clear socket so the persisted record is unambiguous.
		ac.Socket = ""
	case "unix", "npipe":
		ac.Socket = strings.TrimSpace(ac.Socket)
		if ac.Socket == "" {
			return fmt.Errorf("socket path is required for scheme %q", ac.Scheme)
		}
		// Host/Port are not meaningful for socket-backed apps.
		ac.Host = ""
		ac.Port = 0
	default:
		return errors.New("scheme must be one of http|https|unix|npipe")
	}
	ac.Role = strings.ToLower(strings.TrimSpace(ac.Role))
	if !conf.ValidRole(ac.Role) {
		return fmt.Errorf("role %q must be one of guest|user|admin", ac.Role)
	}
	return nil
}

// outboundList safely returns the manager's view list (or an empty
// slice when no manager is wired, so /api/config never has a null
// field).
func (s *Server) outboundList() []agent.OutboundView {
	if s.deps.Outbound == nil {
		return []agent.OutboundView{}
	}
	return s.deps.Outbound.List()
}

// --- Outbound mounts ---

func (s *Server) handleListOutbound(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"outbound": s.deps.Outbound.List()})
}

type outboundUpsertReq struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	User      string `json:"user"`
	Scheme    string `json:"scheme,omitempty"`
	LocalPort int    `json:"local_port,omitempty"`
}

func (s *Server) handleAddOutbound(c *gin.Context) {
	var req outboundUpsertReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateOutbound(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Local-app and outbound names share the same NoRoute namespace —
	// refuse to register an outbound that would shadow a local app.
	for _, ac := range fc.Apps {
		if strings.EqualFold(ac.Name, req.Path) {
			c.AbortWithStatusJSON(http.StatusConflict,
				gin.H{"error": fmt.Sprintf("path %q collides with custom app of the same name", req.Path)})
			return
		}
	}
	newCfg := conf.OutboundConfig{
		Path:      req.Path,
		Name:      req.Name,
		Host:      req.Host,
		User:      req.User,
		Scheme:    req.Scheme,
		LocalPort: req.LocalPort,
	}
	// A tcp outbound MUST NOT collide on local_port with any other tcp
	// outbound — both would race to bind 127.0.0.1:<port>.
	if newCfg.SchemeNorm() == "tcp" {
		for _, ob := range fc.Outbound {
			if ob.Path == newCfg.Path {
				continue
			}
			if ob.SchemeNorm() == "tcp" && ob.LocalPort == newCfg.LocalPort {
				c.AbortWithStatusJSON(http.StatusConflict,
					gin.H{"error": fmt.Sprintf("local_port %d already used by outbound %q", newCfg.LocalPort, ob.Path)})
				return
			}
		}
	}
	// Upsert by path.
	replaced := false
	for i, ob := range fc.Outbound {
		if ob.Path == req.Path {
			fc.Outbound[i] = newCfg
			replaced = true
			break
		}
	}
	if !replaced {
		fc.Outbound = append(fc.Outbound, newCfg)
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.deps.Outbound.Register(fc.Outbound)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleDeleteOutbound(c *gin.Context) {
	path := c.Param("path")
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	filtered := fc.Outbound[:0]
	for _, ob := range fc.Outbound {
		if ob.Path != path {
			filtered = append(filtered, ob)
		}
	}
	fc.Outbound = filtered
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.deps.Outbound.Register(fc.Outbound)
	c.Status(http.StatusNoContent)
}

type outboundConnectReq struct {
	Password string `json:"password" binding:"required"`
}

func (s *Server) handleConnectOutbound(c *gin.Context) {
	path := c.Param("path")
	if !s.deps.Outbound.Has(path) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "unknown outbound path"})
		return
	}
	var req outboundConnectReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.deps.Outbound.Connect(path, req.Password); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleDisconnectOutbound(c *gin.Context) {
	path := c.Param("path")
	s.deps.Outbound.Disconnect(path)
	c.Status(http.StatusNoContent)
}

// validateOutbound trims and sanity-checks the incoming fields. Path must
// be safe as a URL segment (no slashes/whitespace) AND must not collide
// with the admin UI's reserved paths. Scheme is normalized to "http" or
// "tcp"; tcp requires local_port in [1, 65535].
func validateOutbound(req *outboundUpsertReq) error {
	req.Path = strings.TrimSpace(req.Path)
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	req.User = strings.TrimSpace(req.User)
	if req.Path == "" || req.Name == "" || req.Host == "" || req.User == "" {
		return errors.New("path, name, host, and user are all required")
	}
	if strings.ContainsAny(req.Path, "/ \t") {
		return errors.New("path cannot contain slashes or whitespace")
	}
	switch strings.ToLower(req.Path) {
	case "api", "static", "healthz", "index.html", "app":
		return fmt.Errorf("path %q is reserved by the admin UI", req.Path)
	}
	req.Scheme = strings.ToLower(strings.TrimSpace(req.Scheme))
	switch req.Scheme {
	case "", "http":
		req.Scheme = "" // store empty for back-compat — defaults to "http"
		req.LocalPort = 0
	case "tcp":
		if req.LocalPort < 1 || req.LocalPort > 65535 {
			return fmt.Errorf("local_port %d is out of range (required for scheme tcp)", req.LocalPort)
		}
	default:
		return fmt.Errorf("scheme %q must be one of http|tcp", req.Scheme)
	}
	return nil
}

// scheduleRestart asynchronously triggers the parent's restart closure
// after a short delay so the in-flight HTTP response has time to flush
// AND so multiple back-to-back toggles (the admin UI now auto-saves on
// every switch flip) collapse into a single restart. Each call resets
// the timer; only when ~1s passes without a new call does Restart fire.
func (s *Server) scheduleRestart() {
	if s.deps.Restart == nil {
		return
	}
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	if s.restartTimer != nil {
		s.restartTimer.Stop()
	}
	s.restartTimer = time.AfterFunc(time.Second, s.deps.Restart)
}
