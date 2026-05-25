package adminui

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// respondError maps an admincore.APIError to the matching HTTP status
// code, or 500 when the error isn't an APIError. Single point of
// translation so handlers stay one-liners.
func respondError(c *gin.Context, err error) {
	if ae := admincore.AsAPIError(err); ae != nil {
		c.AbortWithStatusJSON(ae.HTTPStatus(), gin.H{"error": ae.Msg})
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// handleStatus is the SPA's "what should I render?" call.
func (s *Server) handleStatus(c *gin.Context) {
	st, err := s.core.Status()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"configured":      st.Configured,
		"agent_name":      st.AgentName,
		"server_addr":     st.ServerAddr,
		"cloudbox_url":    st.CloudboxURL,
		"current_os_user": st.CurrentOSUser,
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
	sv, err := s.core.SafeView()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, sv)
}

// handleRegister runs the pairing exchange and saves the resulting
// config. Schedules a restart so the new tunnel/identity takes effect.
func (s *Server) handleRegister(c *gin.Context) {
	var req admincore.PairParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := s.core.Pair(c.Request.Context(), req)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         res.OK,
		"restarting": res.RestartPending,
		"agent_name": res.AgentName,
	})
}

// handleBuiltins toggles built-in routes and optional local-daemon
// proxies. Triggers a restart only when the host is already paired.
func (s *Server) handleBuiltins(c *gin.Context) {
	var req admincore.BuiltinsParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := s.core.SetBuiltins(req)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": res.OK, "restarting": res.RestartPending})
}

func (s *Server) handleListApps(c *gin.Context) {
	apps, err := s.core.ListApps()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"apps": apps})
}

// handleUpsertApp validates and persists an app config, mutates the
// live AppRegistry. No restart required.
func (s *Server) handleUpsertApp(c *gin.Context) {
	var req admincore.AppUpsertParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ac, err := s.core.UpsertApp(req)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "app": ac})
}

func (s *Server) handleDeleteApp(c *gin.Context) {
	if err := s.core.DeleteApp(c.Param("name")); err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleRotateProvisioningToken regenerates the per-app bearer the
// cooperating app uses to push grants. Updates both the persisted
// FileConfig and the live registry.
func (s *Server) handleRotateProvisioningToken(c *gin.Context) {
	tok, err := s.core.RotateProvisioningToken(c.Param("name"))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "provisioning_token": tok})
}

func (s *Server) handleRestart(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"ok": true, "restarting": true})
	s.core.ScheduleRestart()
}

// handleSetNetworking persists local_addr / vnc_addr / admin_addr /
// admin_users. Pointer-string semantics on the wire: absent field =
// "leave alone"; explicit empty string = "revert to default at boot".
type setNetworkingReq struct {
	LocalAddr     *string  `json:"local_addr,omitempty"`
	VNCAddr       *string  `json:"vnc_addr,omitempty"`
	AdminAddr     *string  `json:"admin_addr,omitempty"`
	AdminUsers    []string `json:"admin_users,omitempty"`
	SetAdminUsers bool     `json:"set_admin_users,omitempty"`
}

func (s *Server) handleSetNetworking(c *gin.Context) {
	var req setNetworkingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	params := admincore.NetworkingParams{
		LocalAddr: req.LocalAddr,
		VNCAddr:   req.VNCAddr,
		AdminAddr: req.AdminAddr,
	}
	if req.SetAdminUsers {
		u := req.AdminUsers
		if u == nil {
			u = []string{}
		}
		params.AdminUsers = &u
	}
	res, err := s.core.SetNetworking(params)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": res.OK, "restarting": res.RestartPending})
}

// --- Outbound mounts ---

func (s *Server) handleListOutbound(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"outbound": s.core.ListOutbound()})
}

func (s *Server) handleAddOutbound(c *gin.Context) {
	var req admincore.OutboundParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.core.UpsertOutbound(req); err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleDeleteOutbound(c *gin.Context) {
	if err := s.core.DeleteOutbound(c.Param("path")); err != nil {
		respondError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type outboundConnectReq struct {
	Password string `json:"password" binding:"required"`
}

func (s *Server) handleConnectOutbound(c *gin.Context) {
	var req outboundConnectReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.core.ConnectOutbound(c.Param("path"), req.Password); err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleDisconnectOutbound(c *gin.Context) {
	if err := s.core.DisconnectOutbound(c.Param("path")); err != nil {
		respondError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- Cluster (virtual-podman) ---

func (s *Server) handleSetClusterKubeconfig(c *gin.Context) {
	var req admincore.KubeconfigParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := s.core.SetKubeconfig(req)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         res.OK,
		"cluster":    res.Cluster,
		"restarting": res.RestartPending,
	})
}

func (s *Server) handleClearClusterKubeconfig(c *gin.Context) {
	res, err := s.core.ClearKubeconfig()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": res.OK, "restarting": res.RestartPending})
}
