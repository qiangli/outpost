package upgrade

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MountRoute attaches the cloudbox-pushed self-upgrade handler at
// `POST /admin/upgrade` on the given route group.
//
// **Auth model: trust the tunnel.** No bearer check at this layer.
// The route lives on the daemon's matrix-tunnel-fronted main HTTP
// server, which binds 127.0.0.1 only — cloudbox is the only entity
// that can reach it (through the tunnel client's loopback proxy
// port on the cloudbox side). This is the same model the existing
// /apps and /healthz routes use; adding a per-host bearer here would
// require a shared secret cloudbox can present, and the obvious
// candidate (fc.Token aka cfg.MatrixToken) is empty in real
// production deployments where cloudbox runs without a matrix-tunnel
// auth secret. Defense-in-depth lives elsewhere: the AutoUpgrade
// toggle (operator opt-out), the sha256 + envelope.commit checks
// (artifact integrity), and the Probe step (commit-match self-
// verify) all gate what the worker will actually do.
//
// The route is intentionally NOT under `/api/*` — cloudbox calls it
// through the matrix tunnel and the daemon's main HTTP server is
// loopback-only-via-tunnel, so the `/api/*` cookie-auth-protected
// surface (which lives on the separate admin listener at :17777)
// can't reach it anyway. Keeping it at `/admin/upgrade` is purely a
// naming choice — signals "this is a cloudbox→outpost control path,"
// not "this is an admin-UI route."
//
// Status codes: 202 accepted, 200 replay, 400 invalid envelope,
// 403 auto_upgrade off, 304 same commit, 412 min_from mismatch,
// 409 in-flight.
func MountRoute(rg *gin.RouterGroup, w *Worker) {
	if w == nil {
		return
	}
	rg.POST("/admin/upgrade", upgradeHandler(w))
}

func upgradeHandler(w *Worker) gin.HandlerFunc {
	return func(c *gin.Context) {
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
