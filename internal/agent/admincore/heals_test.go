package admincore

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

type healRecorder struct{ *httptest.ResponseRecorder }

func (w *healRecorder) CloseNotify() <-chan bool { return make(chan bool) }

func healRequest(t *testing.T, registry *agent.AppRegistry, accept string) *httptest.ResponseRecorder {
	t.Helper()
	w := &healRecorder{httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/app/app-a/", nil)
	c.Request.Header.Set("Accept", accept)
	c.Request.Header.Set("X-Forwarded-Prefix", "/h/host-a/app/app-a")
	registry.ProxyTo(c, "app-a", "/")
	return w.ResponseRecorder
}

func awaitHeal(t *testing.T, registry *agent.AppRegistry, want string) {
	t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		w := &healRecorder{httptest.NewRecorder()}
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/app/app-a/.outpost/heal", nil)
		registry.ProxyTo(c, "app-a", "/.outpost/heal")
		var st agent.HealStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err == nil && st.State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("heal did not reach %s", want)
}

func TestAppHealWrongAddressAndUndo(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	registry := agent.NewAppRegistry()
	core, err := New(Deps{ConfigPath: filepath.Join(t.TempDir(), "agent.json"), Apps: registry})
	if err != nil {
		t.Fatal(err)
	}
	original := conf.AppConfig{Name: "app-a", Scheme: "http", Host: "203.0.113.7", Port: port, Enabled: true}
	if _, err := core.UpsertApp(AppUpsertParams{AppConfig: original}); err != nil {
		t.Fatal(err)
	}
	first := healRequest(t, registry, "text/html")
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "fixing it") {
		t.Fatalf("first response: %d %s", first.Code, first.Body.String())
	}
	awaitHeal(t, registry, "healed")
	next := healRequest(t, registry, "text/html")
	if next.Code != http.StatusOK || next.Body.String() != "ready" {
		t.Fatalf("next response: %d %s", next.Code, next.Body.String())
	}
	rows, err := core.ListHeals()
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger: %v %v", rows, err)
	}
	app, err := core.UndoHeal(rows[0].ID)
	if err != nil || app.Host != original.Host {
		t.Fatalf("undo: %+v %v", app, err)
	}
}

func TestAppHealNoServiceAndAPIResponse(t *testing.T) {
	registry := agent.NewAppRegistry()
	core, err := New(Deps{ConfigPath: filepath.Join(t.TempDir(), "agent.json"), Apps: registry})
	if err != nil {
		t.Fatal(err)
	}
	app := conf.AppConfig{Name: "app-a", Scheme: "http", Host: "127.0.0.1", Port: 1, Enabled: true}
	if _, err := core.UpsertApp(AppUpsertParams{AppConfig: app}); err != nil {
		t.Fatal(err)
	}
	first := healRequest(t, registry, "application/json")
	if first.Code != http.StatusBadGateway || !strings.Contains(first.Body.String(), "\"incident\"") {
		t.Fatalf("API response: %d %s", first.Code, first.Body.String())
	}
	awaitHeal(t, registry, "failed")
	if st := registryHealStatus(t, registry); st.Attempts > 2 {
		t.Fatalf("too many attempts: %d", st.Attempts)
	}
	rows, err := core.ListHeals()
	if err != nil || len(rows) != 1 || rows[0].Result != "failed" {
		t.Fatalf("ledger: %v %v", rows, err)
	}
	apps, err := core.ListApps()
	if err != nil || apps[0].Host != "127.0.0.1" {
		t.Fatalf("config changed: %v %v", apps, err)
	}
}

func registryHealStatus(t *testing.T, registry *agent.AppRegistry) agent.HealStatus {
	t.Helper()
	w := &healRecorder{httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/app/app-a/.outpost/heal", nil)
	registry.ProxyTo(c, "app-a", "/.outpost/heal")
	var st agent.HealStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAppHealReportsUpstreamCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "broken", 500) }))
	defer upstream.Close()
	registry := agent.NewAppRegistry()
	core, err := New(Deps{ConfigPath: filepath.Join(t.TempDir(), "agent.json"), Apps: registry})
	if err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	var port int
	_, _ = fmt.Sscan(portText, &port)
	if _, err := core.UpsertApp(AppUpsertParams{AppConfig: conf.AppConfig{Name: "app-a", Scheme: "http", Host: host, Port: port, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	resp := healRequest(t, registry, "text/html")
	if resp.Code != 500 {
		t.Fatalf("status %d", resp.Code)
	}
	awaitHeal(t, registry, "reported")
	rows, err := core.ListHeals()
	if err != nil || len(rows) != 1 || rows[0].Kind != "code" {
		t.Fatalf("ledger: %v %v", rows, err)
	}
}

func TestAppHealStartsDeclaredService(t *testing.T) {
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := socket.Addr().(*net.TCPAddr).Port
	_ = socket.Close()
	registry := agent.NewAppRegistry()
	var server *http.Server
	starts := make(chan struct{}, 1)
	ledgerDir := t.TempDir()
	registry.ConfigureHealing(filepath.Join(ledgerDir, "heals.jsonl"), nil, nil,
		func(_ context.Context, app string) (string, string, func(context.Context) error) {
			if app != "app-a" {
				return "", "", nil
			}
			return "unit-a", "stopped", func(context.Context) error {
				listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					return err
				}
				server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ready")) })}
				go server.Serve(listener)
				starts <- struct{}{}
				return nil
			}
		})
	defer func() {
		if server != nil {
			_ = server.Close()
		}
	}()
	if err := registry.RegisterFromConfig(conf.AppConfig{Name: "app-a", Scheme: "http", Host: "127.0.0.1", Port: port, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	first := healRequest(t, registry, "text/html")
	if first.Code != 503 {
		t.Fatalf("first status: %d", first.Code)
	}
	awaitHeal(t, registry, "healed")
	select {
	case <-starts:
	default:
		t.Fatal("declared unit was not started")
	}
	rows, err := agent.ReadHealRecords(filepath.Join(ledgerDir, "heals.jsonl"))
	if err != nil || len(rows) != 1 || rows[0].Rung != "service" {
		t.Fatalf("service ledger: %v %v", rows, err)
	}
	if next := healRequest(t, registry, "text/html"); next.Code != 200 {
		t.Fatalf("next status: %d", next.Code)
	}
}
