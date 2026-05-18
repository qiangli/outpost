package agent

import (
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/gin-gonic/gin"
)

// clipboardMaxBytes caps a single paste-to-remote payload. RFB doesn't
// have a hard limit but multi-megabyte clipboards are virtually always a
// bug or an attack, so we refuse early.
const clipboardMaxBytes = 1 << 20

// clipboardHandler bridges the home host's OS clipboard to/from the cloud.
// GET reads (pbpaste on macOS); POST writes (pbcopy on macOS). The cloud
// gates the route on tier-3 elevation; we trust the proxied request.
//
// Bypasses noVNC's RFB clipboard entirely so it works with macOS Screen
// Sharing's non-standard clipboard handling and on plain-HTTP origins.
func clipboardHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet:
			clipboardRead(c)
		case http.MethodPost:
			clipboardWrite(c)
		default:
			c.AbortWithStatus(http.StatusMethodNotAllowed)
		}
	}
}

func clipboardRead(c *gin.Context) {
	cmd, ok := clipboardReadCmd()
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "clipboard not supported on " + runtime.GOOS})
		return
	}
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("clipboard read", "err", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", out)
}

func clipboardWrite(c *gin.Context) {
	cmd, ok := clipboardWriteCmd()
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "clipboard not supported on " + runtime.GOOS})
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, clipboardMaxBytes+1))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(body) > clipboardMaxBytes {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "clipboard too large"})
		return
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_, _ = stdin.Write(body)
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		slog.Warn("clipboard write", "err", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func clipboardReadCmd() (*exec.Cmd, bool) {
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/pbpaste"), true
	}
	return nil, false
}

func clipboardWriteCmd() (*exec.Cmd, bool) {
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/pbcopy"), true
	}
	return nil, false
}
