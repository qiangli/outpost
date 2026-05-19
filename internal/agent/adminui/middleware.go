package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

const cookieName = "outpost_admin"

// requireSession is the gate the admin API routes hide behind. The gate
// is open until the host has been paired with the portal (AgentName set);
// before that, the listener is loopback-only and the OS user sitting at
// the keyboard who just launched outpost is the only reachable caller.
// Once paired, every API call must carry a valid session cookie minted
// by POST /api/login.
func (s *Server) requireSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.isConfigured() {
			c.Next()
			return
		}
		cookie, err := c.Cookie(cookieName)
		if err != nil || cookie == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "login required"})
			return
		}
		user, ok := s.sessions.Validate(cookie)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		c.Set("admin_user", user)
		c.Next()
	}
}

// isConfigured reports whether the host has been paired (AgentName set).
// Cheap to call — re-reads the file every request, which is fine on
// loopback. Treats read errors as "not configured" so a corrupt or
// unreadable file doesn't accidentally lock the user out of fixing it.
func (s *Server) isConfigured() bool {
	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil || fc == nil {
		return false
	}
	return fc.AgentName != ""
}
