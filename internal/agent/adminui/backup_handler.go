package adminui

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// handleGetBackup returns the persisted BackupConfig. Always 200 with
// an object (empty fields when nothing has been configured) so the
// SPA can render a blank form rather than handling 404.
func (s *Server) handleGetBackup(c *gin.Context) {
	cfg, err := s.core.GetBackup()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// handleSetBackup saves the BackupConfig and re-applies the live
// scheduler. Returns the normalized config (folder paths absolutised)
// so the SPA can reflect what was actually saved.
func (s *Server) handleSetBackup(c *gin.Context) {
	var p admincore.BackupParams
	if err := c.ShouldBindJSON(&p); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	saved, err := s.core.SetBackup(p)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, saved)
}

// handleRunBackup fires the worker once against the currently-applied
// folders, regardless of Enabled. The candidates are returned inline so
// the SPA can render "Run now" results without polling history.
func (s *Server) handleRunBackup(c *gin.Context) {
	out, err := s.core.RunBackupNow(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"candidates": out})
}

// handleBackupHistory returns the last `n` ledger entries (newest
// last). Query param ?limit=N caps the page; default 50.
func (s *Server) handleBackupHistory(c *gin.Context) {
	limit := 50
	if v := c.Query("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	out, err := s.core.BackupHistory(limit)
	if err != nil {
		respondError(c, err)
		return
	}
	if out == nil {
		out = nil // explicit — JSON null is fine; SPA handles both
	}
	c.JSON(http.StatusOK, gin.H{"candidates": out})
}
