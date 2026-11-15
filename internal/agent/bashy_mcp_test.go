package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/qiangli/outpost/internal/agent/svcunit"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

type mcpAppSecrets struct{}

func (mcpAppSecrets) SSOSecret(string) (string, error) { return "unit-test-app-key", nil }

func TestBashyMCPServicePublicationAndTunnelOnly(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
	unit, ok := cat.Service("bashy-mcp")
	if !ok {
		t.Fatal("missing bashy-mcp seed")
	}
	plan, err := svcunit.Plan(unit, service.Active, mcpAppSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Allowed || plan.App == nil || plan.App.Name != "bashy-mcp" || !plan.App.Meta.RequireLogin || !plan.App.Meta.TrustCloudIdentity || len(plan.Mesh) != 0 {
		t.Fatal("unsafe MCP publication")
	}
	if _, err := svcunit.Plan(unit, service.Active, nil); err == nil {
		t.Fatal("publication accepted without secret source")
	}
	received := make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- coopauth.VerifyOutpost(r, []byte("unit-test-app-key"))
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	reg := NewAppRegistry()
	meta := plan.App.Meta
	if err := reg.RegisterWithMeta(plan.App.Name, upstream.URL, AppMeta{RequireLogin: meta.RequireLogin, TrustCloudIdentity: meta.TrustCloudIdentity, SSOSecret: meta.SSOSecret}); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	// Direct ProxyTo is also used by the local admin UI; the escape hatch
	// must not let it mint an MCP identity signature for forged headers.
	router := gin.New()
	router.Any("/app/bashy-mcp/*p", func(c *gin.Context) { reg.ProxyTo(c, "bashy-mcp", c.Param("p")) })
	for _, escape := range []bool{false, true} {
		reg.SetTrustLoopback(escape)
		r := httptest.NewRequest("POST", "/app/bashy-mcp/mcp", nil)
		r.Header.Set("X-Periscope-User", "test-caller")
		r.Header.Set("X-Forwarded-Prefix", "/app/bashy-mcp")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("local escape=%v status=%d", escape, w.Code)
		}
	}
	// A permitted hub caller reaches the app with a verifiable per-app signature.
	reg.SetHostGrants(NewHostGrants("test-caller"))
	r := httptest.NewRequest("POST", "/app/bashy-mcp/mcp", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r = r.WithContext(TunnelIngressConnContext(ctx, nil))
	r.Header.Set("X-Forwarded-Prefix", "/app/bashy-mcp")
	r.Header.Set("X-Periscope-User", "test-caller")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("tunnel status %d", w.Code)
	}
	if !<-received {
		t.Fatal("tunnel identity missing a valid app signature")
	}
}
