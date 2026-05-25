package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// mcpCredentialsView is the SPA's "show me what to paste into .mcp.json"
// payload. Endpoint is the URL agents POST JSON-RPC to; Token is the
// bearer they put in the Authorization header.
type mcpCredentialsView struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

func (s *Server) handleMCPCredentials(c *gin.Context) {
	if s.deps.MCPToken == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "mcp credentials not configured"})
		return
	}
	c.JSON(http.StatusOK, mcpCredentialsView{
		Endpoint: s.deps.MCPEndpoint,
		Token:    s.deps.MCPToken(),
	})
}

func (s *Server) handleRotateMCPToken(c *gin.Context) {
	if s.deps.RotateMCPToken == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "mcp rotation not configured"})
		return
	}
	tok, err := s.deps.RotateMCPToken()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, mcpCredentialsView{
		Endpoint: s.deps.MCPEndpoint,
		Token:    tok,
	})
}
