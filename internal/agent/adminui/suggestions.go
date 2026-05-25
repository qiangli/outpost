package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleListSuggestions returns the apps the operator could enable
// with one click. Probing logic lives in admincore.AppSuggestions; this
// handler is a thin HTTP wrapper around it.
func (s *Server) handleListSuggestions(c *gin.Context) {
	out, err := s.core.AppSuggestions()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"suggestions": out})
}
