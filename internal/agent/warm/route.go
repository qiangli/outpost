package warm

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// MountRoute attaches the cloudbox-driven warm-serving control handler at
// `POST /admin/warm` on rg.
//
// Auth model mirrors `/admin/upgrade`: **no bearer at the HTTP layer.**
// The route lives on the daemon's matrix-tunnel-fronted main HTTP server,
// which binds 127.0.0.1 only, so cloudbox (through the tunnel) is the
// only reachable caller. The route is mounted solely on paired hosts (the
// caller gates on fc.AccessToken != "", same as the upgrade route). What
// the handler actually does is further gated by the live warm budget and
// the busy verdict, so even a spurious call can't override the
// considerate policy.
//
// Body: WarmRequest{model, mode} where mode ∈ load | shard | unload.
// Reply: WarmResponse{status, active_model, busy, warm_budget_bytes}.
func MountRoute(rg *gin.RouterGroup, e *Executor) {
	if e == nil {
		return
	}
	rg.POST("/admin/warm", warmHandler(e))
}

func warmHandler(e *Executor) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req WarmRequest
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad request: " + err.Error()})
			return
		}
		resp, err := e.Apply(c.Request.Context(), req)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) {
				c.JSON(ae.Status, gin.H{"error": ae.Msg, "busy": resp.Busy, "warm_budget_bytes": resp.WarmBudgetBytes})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}
