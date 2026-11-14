package attachsel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func fakeSatellite(t *testing.T, hosts ...string) (*httptest.Server, string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ssh/hosts" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rows := []map[string]string{}
		for _, h := range hosts {
			rows = append(rows, map[string]string{"host": h})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": rows})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return srv, u.Hostname(), port
}

func writeConfig(t *testing.T, atts ...conf.SatelliteAttachment) string {
	t.Helper()
	path := t.TempDir() + "/agent.json"
	if err := conf.SaveFile(path, &conf.FileConfig{Attachments: atts}); err != nil {
		t.Fatal(err)
	}
	return path
}

func att(id, addr string, port int, token string) conf.SatelliteAttachment {
	return conf.SatelliteAttachment{SatelliteID: id, Endpoint: "http://" + addr + ":" + strconv.Itoa(port), Token: token,
		Tunnel: &conf.AttachmentTunnel{AgentName: "me", ServerAddr: addr, ServerPort: port, Protocol: "ws", Token: "t", RemotePort: 7001}}
}

func TestLoadForPicksTheSatelliteThatKnowsTheHost(t *testing.T) {
	_, a1, p1 := fakeSatellite(t, "alpha", "me")
	_, a2, p2 := fakeSatellite(t, "beta", "me")
	path := writeConfig(t, att("sat-1", a1, p1, "tok1"), att("sat-2", a2, p2, "tok2"))
	fc, err := LoadFor(context.Background(), path, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if fc.ServerPort != p2 || fc.AccessToken != "tok2" {
		t.Fatalf("expected the second satellite (port %d), got %+v", p2, fc)
	}
	fc, err = LoadFor(context.Background(), path, "alpha")
	if err != nil || fc.ServerPort != p1 {
		t.Fatalf("expected the first satellite (port %d): %+v %v", p1, fc, err)
	}
}

func TestLoadForFallsBackToTheFirstAttachment(t *testing.T) {
	_, a1, p1 := fakeSatellite(t, "alpha")
	_, a2, p2 := fakeSatellite(t, "beta")
	path := writeConfig(t, att("sat-1", a1, p1, "tok1"), att("sat-2", a2, p2, "tok2"))
	fc, err := LoadFor(context.Background(), path, "nowhere")
	if err != nil || fc.ServerPort != p1 {
		t.Fatalf("unknown host must fall back to attachments[0]: %+v %v", fc, err)
	}
	fc, err = LoadFor(context.Background(), path, "")
	if err != nil || fc.ServerPort != p1 {
		t.Fatalf("empty host must behave like LoadClientFile: %+v %v", fc, err)
	}
}

func TestLoadForLeavesLegacyPairingAlone(t *testing.T) {
	_, a2, p2 := fakeSatellite(t, "beta")
	path := t.TempDir() + "/agent.json"
	if err := conf.SaveFile(path, &conf.FileConfig{AgentName: "old", AccessToken: "a", ServerAddr: "legacy.example", ServerPort: 443,
		Attachments: []conf.SatelliteAttachment{att("sat-2", a2, p2, "tok2")}}); err != nil {
		t.Fatal(err)
	}
	fc, err := LoadFor(context.Background(), path, "beta")
	if err != nil || fc.ServerAddr != "legacy.example" {
		t.Fatalf("legacy pairing must win: %+v %v", fc, err)
	}
}
