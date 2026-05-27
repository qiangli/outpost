package mcpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// newTestMCP builds an mcpapi server backed by an admincore.Server
// pointed at a fresh temp config path. Returns an httptest.Server
// hosting the MCP handler.
func newTestMCP(t *testing.T, token string) (*httptest.Server, *Server) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{}); err != nil {
		t.Fatal(err)
	}
	core, err := admincore.New(admincore.Deps{
		ConfigPath: configPath,
		Apps:       agent.NewAppRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv, err := New(Deps{Core: core, Token: token, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv, mcpSrv
}

// TestBearerAuth — the gate rejects requests without the right token.
func TestBearerAuth(t *testing.T) {
	httpSrv, _ := newTestMCP(t, "deadbeef")
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic c29tZTpwYXNz", http.StatusUnauthorized},
		{"wrong token", "Bearer badbadbad", http.StatusUnauthorized},
		// The good-token case: a valid MCP initialize call would
		// require a streamable-transport POST, which is tested
		// separately. Here we just confirm the auth middleware
		// passes the request through (POST yields 4xx from the MCP
		// handler for missing protocol fields, not 401).
		{"good token", "Bearer deadbeef", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, httpSrv.URL, strings.NewReader("{}"))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if tc.want < 0 {
				// good-token path — anything other than 401 confirms
				// the middleware passed through.
				if resp.StatusCode == http.StatusUnauthorized {
					t.Errorf("good token rejected: status=%d", resp.StatusCode)
				}
				return
			}
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestToolListAndStatusResource exercises the full MCP protocol over
// the streamable HTTP transport: initialize, list tools, read the
// status resource. Confirms the registered tools are visible and that
// admincore wiring resolves end-to-end.
func TestToolListAndStatusResource(t *testing.T) {
	const token = "secret-token-1234"
	httpSrv, _ := newTestMCP(t, token)
	ctx := context.Background()

	transport := &mcp.StreamableClientTransport{
		Endpoint: httpSrv.URL,
		HTTPClient: &http.Client{Transport: &bearerRT{
			token: token,
			base:  http.DefaultTransport,
		}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	// List tools — confirm the parity set is registered.
	wantTools := []string{
		"outpost_pair",
		"outpost_unpair",
		"outpost_set_builtins",
		"outpost_set_networking",
		"outpost_list_apps",
		"outpost_upsert_app",
		"outpost_delete_app",
		"outpost_rotate_app_token",
		"outpost_suggest_apps",
		"outpost_list_outbound",
		"outpost_upsert_outbound",
		"outpost_delete_outbound",
		"outpost_connect_outbound",
		"outpost_disconnect_outbound",
		"outpost_suggest_outbound",
		// outpost_set_kubeconfig (bring-your-own paste) was removed —
		// outposts only join their owning cloudbox's cluster.
		"outpost_clear_kubeconfig",
		"outpost_restart",
		"outpost_rotate_mcp_token",
	}
	got := map[string]bool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("iterating tools: %v", err)
		}
		got[tool.Name] = true
	}
	for _, name := range wantTools {
		if !got[name] {
			t.Errorf("missing tool %q (got: %v)", name, keys(got))
		}
	}

	// Read the status resource — unpaired host should report
	// configured=false.
	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "outpost://status"})
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if len(res.Contents) == 0 {
		t.Fatal("status resource returned no contents")
	}
	body := res.Contents[0].Text
	if !strings.Contains(body, `"configured": false`) {
		t.Errorf("expected configured=false in status; got %s", body)
	}

	// Call outpost_list_apps — should return an empty list for a fresh config.
	apps, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "outpost_list_apps",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call list_apps: %v", err)
	}
	if apps.IsError {
		t.Fatalf("list_apps returned IsError; content=%v", apps.Content)
	}
}

// bearerRT injects the Authorization: Bearer header on every request.
type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
