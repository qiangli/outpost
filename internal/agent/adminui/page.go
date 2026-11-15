package adminui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/yoke/pkg/pagecontrol"
)

// These are the only browser capabilities the admin server will dispatch.
// Networking uses the same endpoint/authority as config set. Credential,
// pairing, login and session controls are intentionally absent: a narrower
// bearer must not borrow an interactive browser's authority through the bridge.
func allowedPageAction(name string) bool {
	switch name {
	case "outpost.navigate", "outpost.shape.open", "outpost.shape-only",
		"outpost.networking.get", "outpost.networking.draft", "outpost.networking.submit":
		return true
	}
	return false
}

func (s *Server) mountPageControl(api *gin.RouterGroup) {
	if s.pages != nil {
		s.pages.Close()
	}
	s.pages = pagecontrol.New(pagecontrol.Options{AllowAction: allowedPageAction})
	s.engine.GET("/page-control.js", func(c *gin.Context) { pagecontrol.ServeScript(c.Writer, c.Request) })
	s.engine.GET("/page-ui.js", func(c *gin.Context) {
		body, err := uiFS.ReadFile("ui/page-ui.js")
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/javascript; charset=utf-8", body)
	})
	api.Any("/page/:operation", s.handlePageControl)
}

func (s *Server) pagePrincipal(c *gin.Context) string {
	if user, ok := c.Get("admin_user"); ok {
		if name, ok := user.(string); ok && name != "" {
			return "outpost:" + name
		}
	}
	// The normal first-run loopback bootstrap has no cookie yet. Bind it to
	// the running OS user, never a browser-provided principal.
	if s.loopbackOnly && !s.isConfigured() {
		if name, err := hostauth.CurrentUser(); err == nil && name != "" {
			return "outpost:" + name
		}
	}
	return ""
}

func (s *Server) handlePageControl(c *gin.Context) {
	principal := s.pagePrincipal(c)
	if principal == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "login required"})
		return
	}
	r := c.Request.Clone(c.Request.Context())
	u := *r.URL
	u.Path = "/" + c.Param("operation")
	r.URL = &u
	r.URL.RawPath = ""
	r.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
	if c.Param("operation") == "events" {
		// A websocket must not remain a logged-in browser after its HTTP session
		// expires, logout revokes it, or the host bearer rotates.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		cookie, _ := c.Cookie(cookieName)
		bearer := viaBearer(c)
		bootstrap := cookie == "" && !bearer
		r = r.WithContext(ctx)
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					valid := false
					if bearer {
						valid = s.bearerMatches(r)
					} else if bootstrap {
						valid = s.loopbackOnly && !s.isConfigured()
					} else {
						user, ok := s.sessions.Validate(cookie)
						valid = ok && principal == "outpost:"+user
					}
					if !valid {
						cancel()
						return
					}
				}
			}
		}()
	}
	// Explicit normalized path; forwarded prefix only affects client URLs.
	s.pages.Serve(c.Writer, r, principal)
}
