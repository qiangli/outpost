package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Control-plane placement handlers.
//
// The token is split across two endpoints on purpose: GET /cluster/control-plane
// never returns it, and GET /cluster/control-plane/token does. An operator
// showing someone their cluster status should not have to think about whether
// a credential is on screen.

func (s *Server) handleGetControlPlane(c *gin.Context) {
	res, err := s.core.ControlPlaneView(false)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) handleSetControlPlane(c *gin.Context) {
	var p admincore.ControlPlaneParams
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	res, err := s.core.SetControlPlane(p)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// handleRevealControlPlaneToken returns the credential workers need to join.
// It is a GET on its own path rather than a field on the status response so
// the value is only ever fetched deliberately.
func (s *Server) handleRevealControlPlaneToken(c *gin.Context) {
	res, err := s.core.ControlPlaneView(true)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// handleRotateControlPlaneToken invalidates every worker's configuration —
// see admincore.RotateControlPlaneToken.
func (s *Server) handleRotateControlPlaneToken(c *gin.Context) {
	res, err := s.core.RotateControlPlaneToken()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}
