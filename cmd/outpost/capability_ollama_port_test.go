package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/ollama"
	"github.com/qiangli/outpost/internal/agent/shard"
)

// TestNonDefaultOllamaUnitPortFlowsThroughPoolShardLAN is the hermetic
// proof for the "no hardcoded service ports" rule: an Ollama unit whose
// daemon lives on a NON-default port (here: an ephemeral loopback stub —
// nothing may assume 11434, and nothing in this test ever dials it) must
// flow through all three consumers:
//
//  1. pool     — the registry push carries the stub's (unique) model
//     inventory, so the watcher provably polled the configured URL;
//  2. shard    — resolveShardAPIPort (the exact resolver newShardManager
//     wires into ManagerConfig.APIPort) returns the stub's port, and the
//     launcher argv serves on that port;
//
// 3. LAN      — the direct-inference listener proxies /v1 to the stub.
func TestNonDefaultOllamaUnitPortFlowsThroughPoolShardLAN(t *testing.T) {
	const stubModel = "hermetic-nondefault-only:q4"

	// The "Ollama daemon": an httptest server on an ephemeral port.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{"name": stubModel, "model": stubModel, "digest": "sha256:hermetic", "size": 1234},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			_, _ = w.Write([]byte("LAN-PROXY-OK"))
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer daemon.Close()
	daemonURL := strings.TrimRight(daemon.URL, "/")
	stubPort := mustPort(t, daemonURL)
	if stubPort == ollama.DefaultPort() {
		t.Fatalf("hermetic stub landed on the default port %d — test would prove nothing", stubPort)
	}

	// The "cloudbox": captures registry pushes.
	pushes := make(chan ollama.RegistryPushPayload, 8)
	cloudbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/llm/registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var p ollama.RegistryPushPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode push: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case pushes <- p:
		default:
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cloudbox.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The ollama unit, configured to the non-default daemon URL — exactly
	// what `bashy service set outpost.ollama --set url=<...>` declares.
	apps := agent.NewAppRegistry()
	oc := &ollamaCapability{Apps: apps}
	ollamaCfg := service.Config{Settings: map[string]string{"url": daemonURL}}
	if err := oc.Activate(ctx, ollamaCfg); err != nil {
		t.Fatalf("activate ollama unit: %v", err)
	}
	defer func() { _ = oc.Deactivate(ctx) }()
	if got := oc.Target(); got != daemonURL {
		t.Fatalf("ollama unit target = %q, want the configured %q", got, daemonURL)
	}

	t.Run("pool pushes the non-default daemon's inventory", func(t *testing.T) {
		pc := &ollamaPoolCapability{
			Ollama:      oc,
			AgentName:   func() string { return "hermetic-host" },
			Cloudbox:    func() string { return cloudbox.URL },
			AccessToken: func() string { return "hermetic-token" },
		}
		if err := pc.Activate(ctx, service.Config{}); err != nil {
			t.Fatalf("activate pool unit: %v", err)
		}
		defer func() { _ = pc.Deactivate(ctx) }()
		select {
		case p := <-pushes:
			found := false
			for _, m := range p.Models {
				if m.Name == stubModel {
					found = true
				}
			}
			if !found {
				t.Fatalf("push carried %v; the stub model %q is missing — the pool polled something other than the configured daemon", modelNames(p.Models), stubModel)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no registry push arrived from the pool unit")
		}
	})

	t.Run("shard orchestration serves on the configured port", func(t *testing.T) {
		port, err := resolveShardAPIPort(oc.Target)
		if err != nil {
			t.Fatalf("resolveShardAPIPort: %v", err)
		}
		if port != stubPort {
			t.Fatalf("resolveShardAPIPort = %d, want the configured daemon port %d", port, stubPort)
		}
		// …and the launcher argv (what a leader-served shard binds) is
		// built from exactly that port.
		ring := shard.Ring{Members: []shard.Member{
			{Rank: 0, Host: "leader", PeerID: "peer-0"},
			{Rank: 1, Host: "worker", PeerID: "peer-1"},
		}}
		plan, err := ring.PlanFor(0)
		if err != nil {
			t.Fatal(err)
		}
		lc := plan.LaunchConfigFor(shard.ServeConfig{Model: stubModel, ServerBin: "prima-server", APIPort: port})
		argv := strings.Join(plan.FullArgs(lc.ModelPath, lc.Extra...), " ")
		if want := "--port " + strconv.Itoa(stubPort); !strings.Contains(argv, want) {
			t.Fatalf("leader argv %q missing %q", argv, want)
		}
		if strings.Contains(argv, "--port "+strconv.Itoa(ollama.DefaultPort())) {
			t.Fatalf("leader argv %q assumed the default ollama port", argv)
		}
	})

	t.Run("LAN inference listener proxies the configured daemon", func(t *testing.T) {
		lc := &lanInferenceCapability{Ollama: oc.Target}
		lanPort := freeLoopbackPort(t)
		if lanPort == ollama.DefaultPort() {
			t.Fatalf("LAN listener would bind the default ollama port %d", lanPort)
		}
		if err := lc.Activate(ctx, service.Config{
			Ports: []fleet.ServicePort{{Name: "http", Port: lanPort}},
		}); err != nil {
			t.Fatalf("activate lan inference unit: %v", err)
		}
		defer func() { _ = lc.Deactivate(ctx) }()

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", lanPort))
		if err != nil {
			t.Fatalf("GET through the LAN listener: %v", err)
		}
		defer resp.Body.Close()
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		if got := strings.TrimSpace(string(body[:n])); got != "LAN-PROXY-OK" {
			t.Fatalf("LAN listener /v1 proxied %q; want the stub's %q (it must target the configured daemon, not a default port)", got, "LAN-PROXY-OK")
		}
	})
}

func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p <= 0 {
		t.Fatalf("url %q carries no usable port", rawURL)
	}
	return p
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func modelNames(ms []ollama.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}
