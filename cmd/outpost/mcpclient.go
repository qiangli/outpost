// MCP client glue for the `outpost apps|builtins|status|unpair` family
// of subcommands. Each subcommand is a thin wrapper that connects to
// the running daemon's /mcp/ endpoint with a bearer token, calls one
// tool / reads one resource, and pretty-prints the result.
//
// Three ways to address the daemon (precedence high → low):
//
//  1. `--host`/`--token` persistent flags on the root command.
//  2. `--remote <name>` persistent flag, pointing at a cached entry in
//     ~/.config/outpost/remotes/<name>.json (written by `outpost
//     remote login <name>`).
//  3. $OUTPOST_HOST / $OUTPOST_ADMIN_ADDR + $OUTPOST_MCP_TOKEN env
//     variables.
//  4. Implicit local: 127.0.0.1:17777 + bearer from the local
//     FileConfig (mode 0600, same OS user). Only this last path
//     requires that the daemon is on the same host as the CLI.
//
// Daemon-not-running: the HTTP dial fails with a friendly error. The
// dedicated `--offline` flag (on a few mutate subcommands) bypasses
// MCP entirely and writes the FileConfig directly via admincore —
// useful for installer scripts.
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

// rootDialOpts is populated by the root command's PersistentPreRun
// from the global --host/--token/--remote flags. Lives at package
// scope because every MCP-routed subcommand reads through dialMCP and
// the cobra plumbing doesn't pass these inputs through any single
// shared closure.
var rootDialOpts struct {
	Host   string
	Token  string
	Remote string
}

// mcpClientSession bundles the SDK session and a cleanup closure.
type mcpClientSession struct {
	session *mcp.ClientSession
	close   func()
}

// dialMCP opens an MCP session against the running daemon. Resolves
// the target endpoint + bearer using the precedence documented at the
// top of this file. Returns a clear error when the daemon isn't
// running OR no bearer is available.
func dialMCP(ctx context.Context) (*mcpClientSession, error) {
	endpoint, token, source, err := resolveMCPTarget()
	if err != nil {
		return nil, err
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &mcpBearerRT{token: token, base: http.DefaultTransport}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "outpost-cli", Version: "v1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, fmt.Errorf("MCP endpoint %s not reachable (%s) — is outpost running there? (`outpost start`)", endpoint, source)
		}
		return nil, fmt.Errorf("connect to MCP at %s: %w", endpoint, err)
	}
	return &mcpClientSession{
		session: session,
		close:   func() { _ = session.Close() },
	}, nil
}

// resolveMCPTarget walks the precedence chain and returns the full
// MCP endpoint URL, the bearer token, and a short human-readable
// label naming where each came from (handy for "not reachable"
// errors so the operator sees whether --host or the local config
// was used). Order: explicit --host/--token > --remote cache >
// $OUTPOST_HOST/$OUTPOST_MCP_TOKEN > local FileConfig.
func resolveMCPTarget() (endpoint, token, source string, err error) {
	addr := strings.TrimSpace(rootDialOpts.Host)
	tok := strings.TrimSpace(rootDialOpts.Token)
	src := []string{}

	// --remote cache: fill in either field if the flag named one but the
	// explicit --host/--token didn't.
	if rootDialOpts.Remote != "" && (addr == "" || tok == "") {
		entry, lerr := loadRemoteEntry(rootDialOpts.Remote)
		if lerr != nil {
			return "", "", "", fmt.Errorf("--remote %q: %w", rootDialOpts.Remote, lerr)
		}
		if addr == "" {
			addr = entry.Addr
		}
		if tok == "" {
			tok = entry.Token
		}
		src = append(src, "--remote "+rootDialOpts.Remote)
	}
	if addr != "" {
		src = append(src, "--host")
	}
	if tok != "" {
		src = append(src, "--token")
	}

	// Env: only fill blanks left after the flag pass.
	if addr == "" {
		if v := strings.TrimSpace(os.Getenv("OUTPOST_HOST")); v != "" {
			addr = v
			src = append(src, "$OUTPOST_HOST")
		} else if v := strings.TrimSpace(os.Getenv("OUTPOST_ADMIN_ADDR")); v != "" {
			addr = v
			src = append(src, "$OUTPOST_ADMIN_ADDR")
		}
	}
	if tok == "" {
		if v := strings.TrimSpace(os.Getenv("OUTPOST_MCP_TOKEN")); v != "" {
			tok = v
			src = append(src, "$OUTPOST_MCP_TOKEN")
		}
	}

	// Local FileConfig fallback for the bearer. Only consulted when
	// nothing higher in the chain supplied one.
	if tok == "" {
		cfgPath, err := conf.DefaultConfigPath()
		if err != nil {
			return "", "", "", fmt.Errorf("locate config path: %w", err)
		}
		fc, err := conf.LoadFile(cfgPath)
		if err != nil {
			return "", "", "", fmt.Errorf("read config: %w", err)
		}
		if fc == nil || strings.TrimSpace(fc.MCPBearerToken) == "" {
			return "", "", "", fmt.Errorf("no MCP bearer token — run `outpost start` once on this machine, or pass --token / $OUTPOST_MCP_TOKEN / --remote <name>")
		}
		tok = fc.MCPBearerToken
		src = append(src, cfgPath)
	}

	// Local addr fallback.
	if addr == "" {
		addr = "127.0.0.1:17777"
		src = append(src, "default 127.0.0.1:17777")
	}

	endpoint = normalizeMCPEndpoint(addr)
	return endpoint, tok, strings.Join(src, ", "), nil
}

// localMCPEndpointURL returns the local-machine MCP endpoint as a
// best-effort string for surfaces like `outpost mcp endpoint` that
// print what the operator should paste into a .mcp.json on this
// machine. Always loopback-targeted (env override accepted for
// admin-listener relocation).
func localMCPEndpointURL() string {
	addr := strings.TrimSpace(os.Getenv("OUTPOST_ADMIN_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:17777"
	}
	return normalizeMCPEndpoint(addr)
}

// normalizeMCPEndpoint accepts any of: "host:port", "127.0.0.1:17777",
// "http://host.local:17777", "https://outpost.example.com/" and
// returns the full ".../mcp" URL the SDK expects.
func normalizeMCPEndpoint(addr string) string {
	a := strings.TrimSpace(addr)
	a = strings.TrimRight(a, "/")
	if !strings.HasPrefix(a, "http://") && !strings.HasPrefix(a, "https://") {
		a = "http://" + a
	}
	if !strings.HasSuffix(a, "/mcp") {
		a += "/mcp"
	}
	return a
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
