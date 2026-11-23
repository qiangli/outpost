package agent

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// TestSiteMCPLiveFront serves the real app proxy as a site's front for MCP
// client acceptance (opt-in: OUTPOST_SITE_MCP_FRONT=ADDR). The cloud's part
// is stood in for: it forwards X-Periscope-User as sent and stamps the app
// mount as X-Forwarded-Prefix, exactly what the hosted plane does after its
// session check. Everything after that is the production path: the
// require_login gate, the identity and relationship stamp, the /mcp proxy
// and the protected-resource metadata.
//
//	OUTPOST_SITE_MCP_FRONT        listen address of the front
//	OUTPOST_SITE_MCP_PAGES_PORT   loopback port of the site's pages server
//	OUTPOST_SITE_MCP_ENGINE_PORT  loopback port of bashy mcp serve --tools site
//	OUTPOST_SITE_MCP_SECRET       the app's signing key (agent.json sso_secret)
//	OUTPOST_SITE_MCP_OWNER / _MEMBER  host owner and one member
//	OUTPOST_SITE_MCP_SECONDS      how long to serve (default 600)
func TestSiteMCPLiveFront(t *testing.T) {
	addr := os.Getenv("OUTPOST_SITE_MCP_FRONT")
	if addr == "" {
		t.Skip("set OUTPOST_SITE_MCP_FRONT to serve a live site MCP front")
	}
	port := func(key string) int {
		n, err := strconv.Atoi(os.Getenv(key))
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return n
	}
	reg := NewAppRegistry()
	reg.SetTrustLoopback(true)
	if err := reg.RegisterFromConfig(conf.AppConfig{Name: "site", Scheme: "http", Port: port("OUTPOST_SITE_MCP_PAGES_PORT"),
		Enabled: true, TrustCloudIdentity: true, SSOSecret: os.Getenv("OUTPOST_SITE_MCP_SECRET"),
		MCPPort: port("OUTPOST_SITE_MCP_ENGINE_PORT")}); err != nil {
		t.Fatal(err)
	}
	grants := NewHostGrants(os.Getenv("OUTPOST_SITE_MCP_OWNER"))
	grants.Update("live", &GrantCopy{Owner: os.Getenv("OUTPOST_SITE_MCP_OWNER"),
		Members: []GrantMember{{Email: os.Getenv("OUTPOST_SITE_MCP_MEMBER"), Circle: "friends"}}})
	reg.SetHostGrants(grants)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Any("/app/:name/*p", func(c *gin.Context) {
		c.Request.Header.Set("X-Forwarded-Prefix", "/app/"+c.Param("name"))
		reg.ProxyTo(c, c.Param("name"), c.Param("p"))
	})
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	seconds := 600
	if v := os.Getenv("OUTPOST_SITE_MCP_SECONDS"); v != "" {
		seconds, _ = strconv.Atoi(v)
	}
	srv := &http.Server{Handler: engine, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(l)
	t.Logf("site front on http://%s/app/site/ for %ds", l.Addr(), seconds)
	time.Sleep(time.Duration(seconds) * time.Second)
	_ = srv.Close()
}
