package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/recovery"
)

// A loopback relay models a network adapter losing and regaining the tunnel
// path. The tunnel client and the server stay running throughout the test.
func TestTunnelRecoversFromNetworkBounce(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "origin")
	}))
	defer origin.Close()

	const token = "fixture-token"
	serverPort, proxyPort := freePort(t), freePort(t)
	server, err := NewTunnelServer(TunnelServerConfig{BindAddr: "127.0.0.1", BindPort: serverPort, Token: token}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go server.Run(ctx)
	defer server.Close()

	relay, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	var mu sync.Mutex
	available := true
	connections := map[net.Conn]net.Conn{}
	setAvailable := func(up bool) {
		mu.Lock()
		defer mu.Unlock()
		available = up
		if !up {
			for client, upstream := range connections {
				client.Close()
				upstream.Close()
			}
		}
	}
	defer setAvailable(false)
	go func() {
		for {
			client, err := relay.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if !available {
				mu.Unlock()
				client.Close()
				continue
			}
			upstream, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", serverPort), time.Second)
			if err != nil {
				mu.Unlock()
				client.Close()
				continue
			}
			connections[client] = upstream
			mu.Unlock()
			go bridgeTestTunnel(client, upstream, func() {
				mu.Lock()
				delete(connections, client)
				mu.Unlock()
			})
		}
	}()

	tunnel, err := NewTunnel(TunnelConfig{
		ServerAddr: "127.0.0.1", ServerPort: relay.Addr().(*net.TCPAddr).Port, Token: token, User: "fixture-user",
	}, []TCPProxy{{Name: "fixture", LocalPort: origin.Listener.Addr().(*net.TCPAddr).Port, RemotePort: proxyPort}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tunnelDone := make(chan error, 1)
	go func() { tunnelDone <- tunnel.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-tunnelDone; err != nil {
			t.Error(err)
		}
	}()

	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	tunnelURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	originAvailable := func() bool {
		r, err := client.Get(tunnelURL)
		if err != nil {
			return false
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		return err == nil && r.StatusCode == 200 && string(body) == "origin"
	}
	waitTunnelFixture(t, ctx, "initial origin traffic", originAvailable)
	// Allow the status poll to observe the running proxy before cutting it.
	time.Sleep(1200 * time.Millisecond)
	setAvailable(false)
	waitTunnelFixture(t, ctx, "transport_lost event", func() bool { return sawTunnelRecoveryState("down") })
	if originAvailable() {
		t.Fatal("the origin was reachable through the interrupted network path")
	}
	setAvailable(true)
	waitTunnelFixture(t, ctx, "automatic tunnel recovery", originAvailable)
	waitTunnelFixture(t, ctx, "matching recovered event", func() bool { return sawTunnelRecoveryState("recovered") })

	events, err := recovery.Read()
	if err != nil || len(events) != 2 || events[0].State != "down" || events[1].State != "recovered" {
		t.Fatalf("network bounce events = %+v, error = %v", events, err)
	}
}

func waitTunnelFixture(t *testing.T, ctx context.Context, description string, ready func() bool) {
	t.Helper()
	for !ready() {
		select {
		case <-ctx.Done():
			events, _ := recovery.Read()
			t.Fatalf("waiting for %s: %v (events %+v)", description, ctx.Err(), events)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func sawTunnelRecoveryState(state string) bool {
	events, _ := recovery.Read()
	for _, event := range events {
		if event.Component == "tunnel" && event.State == state && event.Reason == "transport_lost" {
			return true
		}
	}
	return false
}

func bridgeTestTunnel(client, upstream net.Conn, cleanup func()) {
	defer cleanup()
	defer client.Close()
	defer upstream.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-done
}
