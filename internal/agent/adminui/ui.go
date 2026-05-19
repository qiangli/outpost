package adminui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed ui
var uiFS embed.FS

// mountUI serves the embedded SPA at "/" and any nested static assets at
// their natural paths. Single-page app: the SPA reads /api/status on load
// to decide whether to render the login form, the pairing form, or the
// full admin surface.
func (s *Server) mountUI() {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		// Compile-time embed guarantees this never happens at runtime;
		// keep the fallback so a future restructure fails loudly.
		s.engine.GET("/", func(c *gin.Context) {
			c.String(http.StatusInternalServerError, "admin UI assets missing")
		})
		return
	}
	files := http.FS(sub)
	handler := http.FileServer(files)
	s.engine.GET("/", gin.WrapH(handler))
	s.engine.GET("/index.html", gin.WrapH(handler))
	s.engine.GET("/static/*filepath", gin.WrapH(handler))
}
