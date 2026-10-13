package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The shard plane must never pin the loopback Ollama default: on a host
// whose daemon lives elsewhere (a $OLLAMA_HOST daemon, bashy's own engine,
// or the live ollama unit's detected target) the old hardcoded
// http://127.0.0.1:11434 pulled — and inventoried — the wrong daemon.
func TestResolveShardOllamaURLExplicitWins(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	t.Setenv("BASHY_OLLAMA_PORT", "0")
	unit := func() string { return "http://unit-target:11434" }
	if got := resolveShardOllamaURL("http://explicit:9999", unit); got != "http://explicit:9999" {
		t.Errorf("explicit override = %q, want the explicit URL", got)
	}
}

// The live ollama unit's target beats every fallback: it already folds in
// that unit's settings.url / $OLLAMA_HOST / detection.
func TestResolveShardOllamaURLUnitTargetWins(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	t.Setenv("BASHY_OLLAMA_PORT", "12099")
	unit := func() string { return "http://unit-target:11434" }
	if got := resolveShardOllamaURL("", unit); got != "http://unit-target:11434" {
		t.Errorf("unit target = %q, want the unit target", got)
	}
}

// An answering host daemon ($OLLAMA_HOST-aware) beats bashy's engine,
// mirroring the ollama unit's own preference for the host daemon.
func TestResolveShardOllamaURLPrefersAnsweringHostDaemon(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer host.Close()
	t.Setenv("OLLAMA_HOST", host.URL)
	t.Setenv("BASHY_OLLAMA_PORT", "12099")
	if got := resolveShardOllamaURL("", nil); got != host.URL {
		t.Errorf("got %q, want the answering host daemon %q", got, host.URL)
	}
}

// With the host daemon down, the shard plane falls back to bashy's own
// engine instead of pulling from a dead loopback.
func TestResolveShardOllamaURLFallsBackToBashyEngine(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1") // closed port: refused fast
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer engine.Close()
	u, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_OLLAMA_PORT", port)
	want := "http://127.0.0.1:" + port
	if got := resolveShardOllamaURL("", nil); got != want {
		t.Errorf("got %q, want bashy's engine %q", got, want)
	}
}

// An explicit $OLLAMA_HOST survives even when nothing answers, so a pull
// attempt still targets the operator's daemon rather than a hardcoded
// loopback.
func TestResolveShardOllamaURLPreservesHostOverrideWhenNothingAnswers(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	t.Setenv("BASHY_OLLAMA_PORT", "0") // ephemeral: no stable engine URL
	if got := resolveShardOllamaURL("", nil); got != "http://127.0.0.1:1" {
		t.Errorf("got %q, want the $OLLAMA_HOST daemon", got)
	}
}

// With no override, unit or stable engine, an implicit 11434 would silently
// route to the wrong daemon. Provisioning must report the missing endpoint.
func TestResolveShardOllamaURLWithoutEndpoint(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("BASHY_OLLAMA_PORT", "0")
	if got := resolveShardOllamaURL("", nil); got != "" {
		t.Errorf("got %q, want no endpoint rather than an implicit 11434", got)
	}
}

func TestEnsureModelWithoutEndpointReportsConfiguration(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	_, err := ensureModel(context.Background(), "", "missing-model", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no Ollama endpoint configured") {
		t.Fatalf("ensureModel error = %v, want missing endpoint diagnostic", err)
	}
}

// settings.ollama_url is the shard unit's explicit override; empty stays
// empty so the resolver falls through to the unit/engine chain.
func TestReadShardSettingsOllamaURL(t *testing.T) {
	ss := readShardSettings(cfgOf("shard", map[string]string{"ollama_url": "http://ollama-lan:11434"}))
	if ss.OllamaURL != "http://ollama-lan:11434" {
		t.Errorf("OllamaURL = %q", ss.OllamaURL)
	}
	if zero := readShardSettings(cfgOf("shard", nil)); zero.OllamaURL != "" {
		t.Errorf("empty declaration OllamaURL = %q, want empty", zero.OllamaURL)
	}
}

// Model acquisition pulls through the resolved daemon URL, not loopback.
func TestEnsureModelPullsThroughResolvedURL(t *testing.T) {
	var pulled bool
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pull" {
			pulled = true
		}
	}))
	defer stub.Close()

	t.Setenv("OLLAMA_MODELS", t.TempDir()) // no manifests: tier (c) pull runs

	_, err := ensureModel(context.Background(), stub.URL, "regression-test-model", nil, nil)
	if err == nil {
		t.Fatal("ensureModel with an empty model store must fail at manifest resolution")
	}
	if !pulled {
		t.Errorf("no POST /api/pull reached the resolved daemon at %s (want the pull, not a hardcoded loopback)", stub.URL)
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("provision error = %v, want the manifest-resolution failure after a successful pull", err)
	}
}
