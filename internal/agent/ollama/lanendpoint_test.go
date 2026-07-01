package ollama

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFirstPrivateIPv4(t *testing.T) {
	mustCIDR := func(s string) *net.IPNet {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", s, err)
		}
		// ParseCIDR zeroes the host bits in n.IP; keep the host address.
		ip, _, _ := net.ParseCIDR(s)
		n.IP = ip
		return n
	}
	ipnet := func(s string) net.Addr { return mustCIDR(s) }

	tests := []struct {
		name  string
		addrs []net.Addr
		want  string
	}{
		{
			name:  "picks first rfc1918 (192.168) skipping loopback",
			addrs: []net.Addr{ipnet("127.0.0.1/8"), ipnet("192.168.1.20/24")},
			want:  "192.168.1.20",
		},
		{
			name:  "10/8 private",
			addrs: []net.Addr{ipnet("10.4.5.6/8")},
			want:  "10.4.5.6",
		},
		{
			name:  "172.16/12 private",
			addrs: []net.Addr{ipnet("172.20.0.9/16")},
			want:  "172.20.0.9",
		},
		{
			name:  "172.15 is NOT private (below 172.16)",
			addrs: []net.Addr{ipnet("172.15.0.1/16")},
			want:  "",
		},
		{
			name:  "172.32 is NOT private (above 172.31)",
			addrs: []net.Addr{ipnet("172.32.0.1/16")},
			want:  "",
		},
		{
			name:  "public address ignored",
			addrs: []net.Addr{ipnet("8.8.8.8/24")},
			want:  "",
		},
		{
			name:  "skips public, returns the private one",
			addrs: []net.Addr{ipnet("203.0.113.7/24"), ipnet("192.168.50.4/24")},
			want:  "192.168.50.4",
		},
		{
			name:  "IPAddr form supported",
			addrs: []net.Addr{&net.IPAddr{IP: net.ParseIP("10.1.2.3")}},
			want:  "10.1.2.3",
		},
		{
			name:  "empty",
			addrs: nil,
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstPrivateIPv4(tt.addrs); got != tt.want {
				t.Errorf("firstPrivateIPv4()=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestLANEndpoint(t *testing.T) {
	// Whatever the host reports, the result is either "" (no private LAN
	// IPv4) or a well-formed http://<ip>:<port>/v1 — assert the shape when
	// present so the contract with cloudbox is stable.
	got := LANEndpoint(11435)
	if got == "" {
		return // no private LAN IPv4 on this machine — acceptable
	}
	if !hasPrefix(got, "http://") || !hasSuffix(got, ":11435/v1") {
		t.Errorf("LANEndpoint(11435)=%q, want http://<ip>:11435/v1", got)
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func hasSuffix(s, p string) bool { return len(s) >= len(p) && s[len(s)-len(p):] == p }

// TestWatcher_PushLANEndpoint asserts the registry push carries lan_endpoint
// when Config.LANEndpoint is set, and omits it (empty) when it isn't.
func TestWatcher_PushLANEndpoint(t *testing.T) {
	body := `{"models":[{"name":"a","digest":"d"}]}`

	run := func(t *testing.T, lanEndpoint string) RegistryPushPayload {
		t.Helper()
		ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				_, _ = io.WriteString(w, body)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(ollamaSrv.Close)
		reg := &capturingRegistry{}
		cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/llm/registry" {
				http.NotFound(w, r)
				return
			}
			reg.ServeHTTP(w, r)
		}))
		t.Cleanup(cloudSrv.Close)

		w, err := New(Config{
			AgentName:         "test-agent",
			Version:           "abc1234",
			OllamaURL:         ollamaSrv.URL,
			CloudboxURL:       cloudSrv.URL,
			AccessToken:       "TOKEN",
			LANEndpoint:       lanEndpoint,
			PollInterval:      20 * time.Millisecond,
			HeartbeatInterval: time.Hour,
			HTTPClient:        cloudSrv.Client(),
			Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && reg.calls.Load() < 1 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done
		last, ok := reg.lastPayload()
		if !ok {
			t.Fatal("no payload captured")
		}
		return last
	}

	t.Run("on", func(t *testing.T) {
		last := run(t, "http://192.0.2.10:11435/v1")
		if last.LANEndpoint != "http://192.0.2.10:11435/v1" {
			t.Errorf("LANEndpoint=%q, want http://192.0.2.10:11435/v1", last.LANEndpoint)
		}
	})
	t.Run("off", func(t *testing.T) {
		last := run(t, "")
		if last.LANEndpoint != "" {
			t.Errorf("LANEndpoint=%q, want empty (omitted)", last.LANEndpoint)
		}
	})
}
