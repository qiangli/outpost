// Package otel discovers the local `ycode serve` observability stack
// (Prometheus + Alertmanager + VictoriaLogs + Jaeger + Perses, all
// reverse-proxied under one bearer-authed HTTP server) and lets outpost
// expose each surface through the matrix tunnel as a built-in app.
//
// The data plane stays on the outpost: cloudbox federates by fanning
// out queries to each paired host's /app/otel-* surface; nothing is
// shipped or stored centrally. Symmetric with the LLM-pool and k3s-
// agent legs, where cloudbox owns the control plane and outposts own
// the data.
//
// Discovery mirrors internal/agent/ycode/: a running `ycode serve`
// publishes $HOME/.agents/ycode/manifest.json. The manifest carries the
// proxy base URL (endpoints.proxy) and the path to the bearer token
// file (auth.tokenFile). One ycode-serve per OS user — no scanning,
// no probing beyond a single file read.
package otel

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Target describes a discovered ycode observability proxy. ProxyURL is
// the base URL with no trailing slash (e.g. "http://127.0.0.1:31415");
// callers append the per-surface sub-path (e.g. "/prometheus/").
// Token is the raw bearer string read from auth.tokenFile, or "" when
// the manifest didn't advertise one (auth disabled).
//
// Available reports whether a probe of ProxyURL returned any HTTP
// response. False means either no manifest, no ycode process, or a
// dead process (stale manifest); the caller skips registration.
type Target struct {
	ProxyURL  string
	Token     string
	Available bool
	// ManifestPath is the file we read (or tried to read). Exposed for
	// admin-UI grey-out text ("tried <path>").
	ManifestPath string
}

const probeTimeout = 500 * time.Millisecond

// Detect reads the ycode manifest, extracts the proxy URL + bearer
// token, and verifies the proxy is alive. Safe to call concurrently;
// no state mutated. Returns a zero-value Target with Available=false
// when ycode isn't running.
func Detect() Target {
	t := Target{ManifestPath: defaultManifestPath()}
	if t.ManifestPath == "" {
		return detectBashyStack(t)
	}
	b, err := os.ReadFile(t.ManifestPath)
	if err != nil {
		return detectBashyStack(t)
	}
	var m struct {
		Auth struct {
			TokenFile string `json:"tokenFile"`
		} `json:"auth"`
		Endpoints struct {
			Proxy string `json:"proxy"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return t
	}
	t.ProxyURL = strings.TrimRight(strings.TrimSpace(m.Endpoints.Proxy), "/")
	if t.ProxyURL == "" {
		return t
	}
	if tf := strings.TrimSpace(m.Auth.TokenFile); tf != "" {
		if tok, err := os.ReadFile(tf); err == nil {
			t.Token = strings.TrimSpace(string(tok))
		}
	}
	t.Available = httpAlive(t.ProxyURL)
	if !t.Available {
		return detectBashyStack(t)
	}
	return t
}

// detectBashyStack finds the observability stack where it lives TODAY.
//
// The stack moved from `ycode serve` to bashy, but this package still looked
// only for ycode's manifest. So on every host the manifest was absent, otel
// reported unavailable, and the built-in stayed off — including on a host that
// had been running the stack continuously for eight days. Discovery that keys
// on a file the producer no longer writes reports "not installed" for something
// that is installed and running.
//
// bashy's stack advertises no manifest: it is a fixed proxy port, and the
// canonical way to locate it is the same one `bashy otel --url` uses —
// $BASHY_OTEL_QUERY_URL, else the default proxy port. So probe that directly.
// No token: the stack binds loopback and does not authenticate.
//
// Kept as a fallback rather than a replacement so a host still running the
// older ycode-served stack keeps working.
func detectBashyStack(t Target) Target {
	url := strings.TrimRight(strings.TrimSpace(os.Getenv("BASHY_OTEL_QUERY_URL")), "/")
	if url == "" {
		url = defaultBashyProxyURL
	}
	if !httpAlive(url) {
		return t
	}
	t.ProxyURL = url
	t.Token = ""
	t.Available = true
	return t
}

// defaultBashyProxyURL is the stack's default proxy/UI port. It carries OTLP
// ingest and the query surfaces on one port, which is why there is nothing to
// discover beyond reaching it.
const defaultBashyProxyURL = "http://127.0.0.1:31415"

func defaultManifestPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "ycode", "manifest.json")
}

func httpAlive(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode > 0
}
