package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// vncCreds is the first WebSocket frame the browser sends — a JSON object
// with the user's macOS account credentials. The agent uses them to perform
// the RFB auth handshake against the local VNC server; the browser-facing
// half of the WebSocket then runs as auth-type None. Doing it this way
// means the browser never needs window.crypto.subtle, so Periscope works
// over plain-HTTP / LAN-IP origins (where isSecureContext is false).
type vncCreds struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// desktopHandler terminates RFB auth in the agent and byte-splices the
// post-init stream between the browser WebSocket and the local VNC server.
//
// vncAddr defaults to 127.0.0.1:5900 (macOS Screen Sharing).
func desktopHandler(vncAddr string) gin.HandlerFunc {
	if vncAddr == "" {
		vncAddr = "127.0.0.1:5900"
	}
	return func(c *gin.Context) {
		ws, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			Subprotocols:       []string{"binary"},
		})
		if err != nil {
			slog.Warn("desktop ws accept", "err", err)
			return
		}
		defer ws.Close(websocket.StatusInternalError, "closing")

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		reader := &wsReader{ctx: ctx, ws: ws}

		credCtx, credCancel := context.WithTimeout(ctx, 30*time.Second)
		_, raw, err := ws.Read(credCtx)
		credCancel()
		if err != nil {
			slog.Warn("desktop read creds", "err", err)
			_ = ws.Close(websocket.StatusPolicyViolation, "missing credentials")
			return
		}
		var creds vncCreds
		if err := json.Unmarshal(raw, &creds); err != nil || creds.Password == "" {
			_ = ws.Close(websocket.StatusPolicyViolation, "bad credentials frame")
			return
		}

		dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
		conn, serverInit, err := vncDialAuth(dialCtx, vncAddr, creds.User, creds.Password)
		dialCancel()
		if err != nil {
			slog.Warn("desktop vnc auth", "addr", vncAddr, "err", err)
			_ = ws.Close(websocket.StatusPolicyViolation, err.Error())
			return
		}
		defer conn.Close()

		if err := vncServeNoAuth(ctx, ws, reader, serverInit); err != nil {
			slog.Warn("desktop browser handshake", "err", err)
			_ = ws.Close(websocket.StatusInternalError, "browser handshake failed")
			return
		}

		// VNC → WS.
		go func() {
			defer cancel()
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		// WS → VNC. wsReader may still hold bytes left over from the last
		// browser frame during handshake; reading via the same reader drains
		// them first before pulling new frames.
		buf := make([]byte, 32*1024)
		for ctx.Err() == nil {
			n, err := reader.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
}
