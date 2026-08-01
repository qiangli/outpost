package adminui

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Peer-plane join handlers — the worker-side twin of control_plane_handler.go.
//
// There is no reveal endpoint here, deliberately. The hosting side owns the
// credentials; a worker only ever needs to know WHETHER it has them, and the
// operator who needs the values reads them on the machine that minted them.

func (s *Server) handleGetClusterJoin(c *gin.Context) {
	res, err := s.core.PeerPlaneView()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) handleSetClusterJoin(c *gin.Context) {
	var p admincore.PeerPlaneParams
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	res, err := s.core.JoinPeerPlane(p)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// handleClearClusterJoin reverts to the cloudbox-hosted plane. It does not
// disable cluster mode — see admincore.LeavePeerPlane.
func (s *Server) handleClearClusterJoin(c *gin.Context) {
	res, err := s.core.LeavePeerPlane()
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}
