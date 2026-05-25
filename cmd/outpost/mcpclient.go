// MCP client glue for the `outpost apps|builtins|status|unpair` family
// of subcommands. Each subcommand is a thin wrapper that connects to
// the running daemon's /mcp/ endpoint with the persisted bearer token,
// calls one tool / reads one resource, and pretty-prints the result.
//
// Auth model: the bearer lives in FileConfig.MCPBearerToken (mode 0600,
// same OS user). The CLI reads it directly from disk — no new admin
// API surface, no separate token. If the file doesn't exist yet, the
// daemon hasn't booted for the first time; the subcommand prints a
// hint to run `outpost start`.
//
// Daemon-not-running: the HTTP dial to 127.0.0.1:17777 fails with a
// friendly error. The dedicated `--offline` flag (on a few mutate
// subcommands) bypasses MCP entirely and writes the FileConfig
// directly via admincore — useful for installer scripts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// mcpClientSession bundles the SDK session and a cleanup closure.
type mcpClientSession struct {
	session *mcp.ClientSession
	close   func()
}

// dialMCP opens an MCP session against the running daemon. Returns a
// clear error when the daemon isn't running OR the FileConfig hasn't
// minted a bearer yet (first-boot before MCP wiring).
func dialMCP(ctx context.Context) (*mcpClientSession, error) {
	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return nil, fmt.Errorf("locate config path: %w", err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if fc == nil || strings.TrimSpace(fc.MCPBearerToken) == "" {
		return nil, fmt.Errorf("no MCP bearer token in %s — run `outpost start` once to generate one", cfgPath)
	}
	endpoint := mcpEndpointURL()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &mcpBearerRT{token: fc.MCPBearerToken, base: http.DefaultTransport}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "outpost-cli", Version: "v1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, fmt.Errorf("MCP endpoint %s not reachable — is outpost running? (`outpost start`)", endpoint)
		}
		return nil, fmt.Errorf("connect to MCP: %w", err)
	}
	return &mcpClientSession{
		session: session,
		close:   func() { _ = session.Close() },
	}, nil
}

func mcpEndpointURL() string {
	addr := strings.TrimSpace(os.Getenv("OUTPOST_ADMIN_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:17777"
	}
	return "http://" + addr + "/mcp"
}

// callTool runs a single tool by name with typed args, decoding the
// structured JSON result into `out`. Returns an error when the tool
// itself reported IsError (with the message), or when the SDK couldn't
// dispatch the call at all.
func (m *mcpClientSession) callTool(ctx context.Context, name string, args, out any) error {
	res, err := m.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return err
	}
	if res.IsError {
		msg := "tool reported error"
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg = tc.Text
				break
			}
		}
		return errors.New(msg)
	}
	if out == nil {
		return nil
	}
	// The SDK exposes structured tool output as `StructuredContent` on
	// the CallToolResult. Round-trip through JSON to materialize the
	// caller's typed struct.
	if res.StructuredContent == nil {
		return nil
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// readResource fetches a JSON resource and decodes the first content
// block into `out`. Resources are MCP's idempotent-fetch primitive —
// callers use them for status / list payloads.
func (m *mcpClientSession) readResource(ctx context.Context, uri string, out any) error {
	res, err := m.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return err
	}
	if len(res.Contents) == 0 {
		return fmt.Errorf("resource %s returned no contents", uri)
	}
	text := res.Contents[0].Text
	if out == nil || text == "" {
		return nil
	}
	return json.Unmarshal([]byte(text), out)
}

// mcpBearerRT injects the Authorization: Bearer header on every
// request. Reused across every CLI MCP call.
type mcpBearerRT struct {
	token string
	base  http.RoundTripper
}

func (b *mcpBearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}
