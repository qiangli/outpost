package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleUpgradeOverview returns the consolidated Update-tab payload —
// build provenance, source, install/run timestamps, update_mode,
// pending envelope (if any), recent ledger entries, rollback
// availability. One round-trip so the SPA tab loads atomically
// rather than fanning out across multiple endpoints.
func (s *Server) handleUpgradeOverview(c *gin.Context) {
	out, err := s.core.UpgradeOverview()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// handleApplyPendingUpgrade — operator clicked Apply on a manual-mode
// queued envelope. Re-runs the persisted envelope through the worker
// with Force=true. Returns the wire Result so the SPA can render
// "Upgrading…" or the appropriate refusal toast.
func (s *Server) handleApplyPendingUpgrade(c *gin.Context) {
	res, err := s.core.ApplyPendingUpgrade(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// handleRollbackUpgrade — operator clicked Rollback. Worker checks
// outpost.previous + probes it before swapping; the response
// surfaces refusal reasons ("no_previous" / "in_flight") so the SPA
// can show a useful toast.
func (s *Server) handleRollbackUpgrade(c *gin.Context) {
	res, err := s.core.RollbackUpgrade(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}
