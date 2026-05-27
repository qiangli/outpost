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

// handleClusterKubeconfigStatus returns the most recent state of
// the on-disk kubectl-ready kubeconfig — path, existence, last
// refresh, error message. The admin UI's Cluster section uses
// this to render "kubectl ready: <path>" or surface a useful error.
func (s *Server) handleClusterKubeconfigStatus(c *gin.Context) {
	c.JSON(http.StatusOK, s.core.UserKubeconfigStatus())
}

// handleClusterKubeconfigRefresh re-mints + rewrites the kubectl-
// ready kubeconfig file. Driven by the "Refresh" button in the
// Cluster section. Always returns 200 + the updated status; failures
// land in Status.LastError so the SPA can render them inline rather
// than as a toast that hides the actionable detail.
func (s *Server) handleClusterKubeconfigRefresh(c *gin.Context) {
	st, err := s.core.RefreshUserKubeconfig(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}
