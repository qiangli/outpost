package upgrade

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// MountRoute attaches the cloudbox-pushed self-upgrade handler at
// `POST /admin/upgrade` on the given route group. The handler is
// gated by `Authorization: Bearer <accessToken>` (constant-time
// compare, same shape as the MCP bearer middleware) and dispatches
// validated envelopes through `w.Apply`.
//
// The route is intentionally NOT under `/api/*` — cloudbox calls it
// through the matrix tunnel and the daemon's main HTTP server is
// loopback-only-via-tunnel, so the `/api/*` cookie-auth-protected
// surface (which lives on the separate admin listener at :17777)
// can't reach it anyway. Keeping it at `/admin/upgrade` is purely a
// naming choice — signals "this is a cloudbox→outpost control path,"
// not "this is an admin-UI route."
//
// Status codes: 202 accepted, 200 replay, 400 invalid envelope, 401
// bad token, 403 auto_upgrade off, 304 same commit, 412 min_from
// mismatch, 409 in-flight.
func MountRoute(rg *gin.RouterGroup, accessToken string, w *Worker) {
	if w == nil || accessToken == "" {
		return
	}
	rg.POST("/admin/upgrade", upgradeHandler(accessToken, w))
}

func upgradeHandler(accessToken string, w *Worker) gin.HandlerFunc {
	expected := []byte(accessToken)
	return func(c *gin.Context) {
		got := strings.TrimSpace(c.GetHeader("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(got, prefix) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "bearer token required"})
			return
		}
		token := strings.TrimSpace(got[len(prefix):])
		if subtle.ConstantTimeCompare([]byte(token), expected) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid bearer token"})
			return
		}

		var env Envelope
		if err := c.BindJSON(&env); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad envelope: " + err.Error()})
			return
		}

		result := w.Apply(c.Request.Context(), env)
		// "invalid" is the only Status value not in HTTPStatus's switch
		// — it stands for envelope validation failure and gets 400.
		code := result.Status.HTTPStatus()
		if result.Status == "invalid" {
			code = http.StatusBadRequest
		}
		c.JSON(code, result)
	}
}
