package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Karmada federation handlers. Status never carries a secret; the token path
// is interactive-login only (denyBearer) like the control-plane token.

func (s *Server) handleGetFederation(c *gin.Context) {
	res, err := s.core.FederationStatus()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) handleSetFederation(c *gin.Context) {
	var body struct {
		admincore.FederationParams
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	var (
		res admincore.FederationResult
		err error
	)
	if body.Enabled != nil && !*body.Enabled {
		res, err = s.core.FederationOff(c.Request.Context(), body.FederationParams)
	} else {
		res, err = s.core.FederationOn(body.FederationParams)
	}
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) handleFederationToken(c *gin.Context) {
	res, err := s.core.FederationToken(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) handleFederationJoin(c *gin.Context) {
	var body admincore.FederationJoinParams
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	res, err := s.core.FederationJoin(body)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) handleFederationLeave(c *gin.Context) {
	var body admincore.FederationParams
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
			return
		}
	}
	res, err := s.core.FederationLeave(c.Request.Context(), body)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}
