package mcpapi

import (
	"context"
	"encoding/json"
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

const (
	fixtureProvToken = "synthetic-prov-token-aaaaaaaaaaaaaaaaaaaaaaaa"
	fixtureSSOSecret = "synthetic-sso-secret-bbbbbbbbbbbbbbbbbbbbbbbb"
	fixtureSvcSecret = "synthetic-service-sso-secret-cccccccccccccc"
)

// `outpost config show --json` and `outpost apps list --json` print these
// resources verbatim, and an agent pulls them wholesale into its context: the
// nested app/service credentials must never be in them.
func TestConfigAndAppsReadsRedactNestedSecrets(t *testing.T) {
	fc := &conf.FileConfig{
		Apps: []conf.AppConfig{{
			Name: "probe", Scheme: "http", Host: "127.0.0.1", Port: 9, Enabled: true,
			TrustCloudIdentity: true, ProvisioningToken: fixtureProvToken, SSOSecret: fixtureSSOSecret,
		}, {
			Name: "plain", Scheme: "http", Host: "127.0.0.1", Port: 10, Enabled: true,
		}},
		BashyServices: []conf.BashyService{{
			Name: "svc", Enabled: true, TrustCloudIdentity: true, SSOSecret: fixtureSvcSecret,
		}},
	}
	session := connectTestMCPWith(t, fc)

	read := func(uri string) string {
		t.Helper()
		res, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		var sb strings.Builder
		for _, c := range res.Contents {
			sb.WriteString(c.Text)
		}
		return sb.String()
	}
	listed, _ := callJSON(t, session, "outpost_list_apps", map[string]any{})
	for name, body := range map[string]string{
		"outpost://config":  read("outpost://config"),
		"outpost://apps":    read("outpost://apps"),
		"outpost_list_apps": listed,
	} {
		for _, secret := range []string{fixtureProvToken, fixtureSSOSecret, fixtureSvcSecret} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked a nested credential", name)
			}
		}
		if !strings.Contains(body, `"probe"`) || !strings.Contains(body, `"trust_cloud_identity"`) {
			t.Errorf("%s lost the non-secret app config: %s", name, body)
		}
	}

	var view admincore.SafeView
	if err := json.Unmarshal([]byte(read("outpost://config")), &view); err != nil {
		t.Fatal(err)
	}
	wantApps := []admincore.SecretPresence{{Name: "probe", HasProvisioningToken: true, HasSSOSecret: true}}
	if len(view.AppSecrets) != 1 || view.AppSecrets[0] != wantApps[0] {
		t.Errorf("app_secrets = %+v, want %+v", view.AppSecrets, wantApps)
	}
	if len(view.BashyServiceSecrets) != 1 || view.BashyServiceSecrets[0] != (admincore.SecretPresence{Name: "svc", HasSSOSecret: true}) {
		t.Errorf("bashy_service_secrets = %+v", view.BashyServiceSecrets)
	}
	if len(view.BashyServices) != 1 || view.BashyServices[0].Name != "svc" || !view.BashyServices[0].TrustCloudIdentity {
		t.Errorf("bashy service config lost: %+v", view.BashyServices)
	}

	// The dedicated secret tool is the one sanctioned read and still works.
	body, isErr := callJSON(t, session, "outpost_get_app_sso_secret", map[string]any{"name": "probe"})
	if isErr || !strings.Contains(body, fixtureSSOSecret) {
		t.Errorf("outpost_get_app_sso_secret no longer returns the secret: %s", body)
	}
}

// connectTestMCPWith opens an authenticated MCP session against a server
// whose agent.json starts as fc.
func connectTestMCPWith(t *testing.T, fc *conf.FileConfig) *mcp.ClientSession {
	t.Helper()
	const token = "secret-token-1234"
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(configPath, fc); err != nil {
		t.Fatal(err)
	}
	core, err := admincore.New(admincore.Deps{ConfigPath: configPath, Apps: agent.NewAppRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv, err := New(Deps{Core: core, Token: token, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(httpSrv.Close)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   httpSrv.URL,
		HTTPClient: &http.Client{Transport: &bearerRT{token: token, base: http.DefaultTransport}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
