package adminui

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

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

// safeView is the redacted FileConfig sent over the API. Token never
// leaves the agent; presence is reported as has_token instead.
type safeView struct {
	AgentName        string            `json:"agent_name"`
	ServerAddr       string            `json:"server_addr"`
	ServerPort       int               `json:"server_port"`
	Protocol         string            `json:"protocol,omitempty"`
	RemotePort       int               `json:"remote_port"`
	AuthURL          string            `json:"auth_url,omitempty"`
	HasToken         bool              `json:"has_token"`
	Apps             []conf.AppConfig  `json:"apps"`
	ShellEnabled     bool              `json:"shell_enabled"`
	DesktopEnabled   bool              `json:"desktop_enabled"`
	ClipboardEnabled bool              `json:"clipboard_enabled"`
	Defaults         map[string]string `json:"defaults"`
}

func toSafeView(fc *conf.FileConfig) safeView {
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
	c.JSON(http.StatusOK, toSafeView(fc))
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
}

// handleBuiltins toggles the built-in shell/desktop/clipboard routes.
// Route mounting happens at boot, so saving here triggers a restart.
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

// handleUpsertApp validates one AppConfig, persists it, and mutates the
// live registry. No restart required — AppRegistry is concurrency-safe.
func (s *Server) handleUpsertApp(c *gin.Context) {
	var ac conf.AppConfig
	if err := c.ShouldBindJSON(&ac); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
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
	ac.Scheme = strings.ToLower(strings.TrimSpace(ac.Scheme))
	if ac.Scheme == "" {
		ac.Scheme = "http"
	}
	if ac.Scheme != "http" && ac.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	ac.Host = strings.TrimSpace(ac.Host)
	if ac.Host == "" {
		ac.Host = "127.0.0.1"
	}
	if ac.Port < 1 || ac.Port > 65535 {
		return fmt.Errorf("port %d is out of range", ac.Port)
	}
	ac.Role = strings.ToLower(strings.TrimSpace(ac.Role))
	if !conf.ValidRole(ac.Role) {
		return fmt.Errorf("role %q must be one of guest|user|admin", ac.Role)
	}
	return nil
}

// scheduleRestart asynchronously triggers the parent's restart closure
// after a short delay so the in-flight HTTP response has time to flush.
// Without this, the browser would see a torn connection mid-response.
func (s *Server) scheduleRestart() {
	if s.deps.Restart == nil {
		return
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		s.deps.Restart()
	}()
}
