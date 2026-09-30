package adminui

import (
	"bytes"
	"embed"
	"html"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
)

//go:embed ui
var uiFS embed.FS

const defaultBaseTag = `<base href="/">`

// mountUI serves the embedded SPA at "/" and any nested static assets at
// their natural paths. Single-page app: the SPA reads api/status on load
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
	index := func(c *gin.Context) {
		body, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			c.String(http.StatusInternalServerError, "admin UI index missing")
			return
		}
		base := forwardedPrefixBase(c.GetHeader("X-Forwarded-Prefix"))
		if base != "/" {
			tag := `<base href="` + html.EscapeString(base) + `">`
			body = bytes.Replace(body, []byte(defaultBaseTag), []byte(tag), 1)
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", body)
	}
	s.engine.GET("/", index)
	s.engine.GET("/index.html", index)
	s.engine.GET("/static/*filepath", gin.WrapH(handler))
}

func forwardedPrefixBase(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || prefix == "/" {
		return "/"
	}
	if !strings.HasPrefix(prefix, "/") || strings.HasPrefix(prefix, "//") {
		return "/"
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "/"
	}
	if path.Clean(prefix) != prefix {
		return "/"
	}
	for _, r := range prefix {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '"' || r == '\'' || r == '<' || r == '>' || r == '\\' || r == '?' || r == '#' {
			return "/"
		}
	}
	return prefix + "/"
}
