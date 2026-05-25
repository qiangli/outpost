package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleOutboundSuggestions calls admincore.OutboundSuggestions (which
// reaches cloudbox's /api/v1/hosts) and returns the flattened list to
// the SPA. Auth requirement (paired host with access_token) is enforced
// inside admincore — a ServiceUnavailable APIError surfaces as 503 here.
func (s *Server) handleOutboundSuggestions(c *gin.Context) {
	out, err := s.core.OutboundSuggestions(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"suggestions": out})
}
