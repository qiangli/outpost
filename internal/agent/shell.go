package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/shell"
)

// shellSizeMsg is the JSON-text-frame schema the browser sends when the
// terminal is resized. Same shape on cloud and agent — the proxy passes
// text frames straight through.
type shellSizeMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// shellHandler is the agent's GET /shell WebSocket endpoint.
//
// Per-connection lifecycle:
//  1. Upgrade the WS.
//  2. Open a PTY + qiangli/sh runner (shell.NewSession).
//  3. Goroutine A: read PTY master → write WS binary frames.
//  4. Goroutine B: read WS frames → text JSON = resize, binary = stdin.
//  5. Runner exits or client disconnects → cancel ctx, close everything.
func shellHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Accept any origin: this endpoint is loopback-only and only reachable
		// through the cloud's WS proxy (which enforces same-origin upstream).
		ws, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			slog.Warn("shell ws accept", "err", err)
			return
		}
		defer ws.Close(websocket.StatusInternalError, "closing")

		sess, err := shell.NewSession()
		if err != nil {
			slog.Error("shell session", "err", err)
			_ = ws.Close(websocket.StatusInternalError, "session")
			return
		}
		defer sess.Close()

		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()

		// Goroutine: PTY master → WS binary frames.
		go func() {
			defer cancel()
			buf := make([]byte, 4096)
			for {
				n, err := sess.Master().Read(buf)
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

		// Goroutine: runner. When it returns the session is dead.
		go func() {
			defer cancel()
			if err := sess.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Info("shell runner exit", "err", err)
			}
		}()

		// Foreground: WS → PTY master.
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageText:
				var m shellSizeMsg
				if err := json.Unmarshal(data, &m); err == nil && m.Type == "size" {
					_ = sess.Resize(m.Cols, m.Rows)
					continue
				}
				// Fall through: text frames that aren't resize get forwarded
				// as stdin (some browsers may send keystrokes as text).
				if _, err := sess.Master().Write(data); err != nil {
					return
				}
			case websocket.MessageBinary:
				if _, err := sess.Master().Write(data); err != nil {
					return
				}
			}
		}
	}
}
