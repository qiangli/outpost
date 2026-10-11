package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/ollama"
)

// The Ollama surfaces as two in-process capabilities, split exactly the way
// the operator thinks about them:
//
//   - `outpost.ollama` — the per-host proxy. Registers the `/app/ollama/*`
//     mount against the DETECTED local daemon, decorates it with the `llm`
//     capability descriptor cloudbox's pool router discovers hosts by, and
//     wires the in-flight counter plus the `/_pool/capacity` intercept.
//   - `outpost.ollama-pool` — participation in cloudbox's multi-host virtual
//     LLM pool. Runs the registry-push watcher that publishes this host's
//     model inventory and capacity. A strict extension of the proxy: its
//     declaration carries `requires: [<the ollama unit>]`, so the monitor
//     orders them and the pool unit reports why it is not up when the proxy
//     is not.
//
// Capabilities, not process units, because the target is detected here (and
// the daemon may be ycode-supplied, bashy-supplied or the operator's own)
// and because the mount's decoration — capability type, proxy wrap,
// intercept — is not expressible in a declaration.

// ollamaCapability is the per-host proxy.
type ollamaCapability struct {
	Apps   *agent.AppRegistry
	Detect func() agent.BuiltinTarget

	mu  sync.Mutex
	url string
	svc *ollama.Service
}

// Target is the local daemon's base URL while the proxy is up, "" otherwise.
// The warm plane, the LAN inference listener and the pool watcher all ask
// for it rather than each re-detecting.
func (c *ollamaCapability) Target() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

// Service is the counter/capacity service the pool watcher shares, nil while
// the proxy is not registered.
func (c *ollamaCapability) Service() *ollama.Service {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.svc
}

// Status is the pool view the admin UI and `outpost status` render. It
// reports not-enabled (rather than failing) while the proxy is down, which
// is the shape those surfaces already expect.
func (c *ollamaCapability) Status() (ollama.PoolStatus, bool) {
	svc := c.Service()
	if svc == nil {
		return ollama.PoolStatus{}, false
	}
	return svc.Status(), true
}

func (c *ollamaCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Apps == nil {
		return fmt.Errorf("ollama: no app registry on this host")
	}
	base, err := c.resolve(cfg)
	if err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("ollama: bad daemon URL %q: %w", base, err)
	}
	port := 0
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	if port == 0 {
		port = 80
	}
	// Withdraw first: AddIntercept APPENDS, so re-activating over a live
	// registration would mount a second /_pool/capacity handler on every
	// Configure pass. Unregister is what clears a name's intercepts.
	if err := c.Deactivate(ctx); err != nil {
		return err
	}
	if err := c.Apps.RegisterFromConfig(conf.AppConfig{
		Name: agent.BuiltinOllama, Scheme: u.Scheme, Host: u.Hostname(), Port: port,
		Role: "admin", Enabled: true,
	}); err != nil {
		return fmt.Errorf("ollama: register app: %w", err)
	}
	// The counter is wired unconditionally: it just observes locally (one
	// atomic per generation request, invisible next to Ollama's own
	// latency), and the pool unit is what actually publishes.
	svc := ollama.NewService(ollama.NewCounter())
	c.Apps.SetCapabilities(agent.BuiltinOllama, &agent.AppCapabilities{Type: "llm"})
	c.Apps.SetProxyWrap(agent.BuiltinOllama, svc.WrapProxy)
	c.Apps.AddIntercept(agent.BuiltinOllama, "/_pool/capacity", svc.CapacityHandler())
	c.mu.Lock()
	c.url, c.svc = base, svc
	c.mu.Unlock()
	slog.Info("ollama: proxying the local daemon", "target", base)
	return nil
}

// Configure re-resolves the target and re-registers. A changed URL moves the
// mount live; a fresh counter is deliberate (the old one counted requests to
// a daemon that is no longer the target).
func (c *ollamaCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *ollamaCapability) Deactivate(context.Context) error {
	c.mu.Lock()
	had := c.url != ""
	c.url, c.svc = "", nil
	c.mu.Unlock()
	if had && c.Apps != nil {
		c.Apps.SetProxyWrap(agent.BuiltinOllama, nil)
		c.Apps.Unregister(agent.BuiltinOllama)
	}
	return nil
}

// Health asks the daemon for its model list. A daemon the operator stopped
// takes the mount's route and the `llm` advertisement with it, instead of
// leaving cloudbox routing inference at a host that cannot serve it.
func (c *ollamaCapability) Health(ctx context.Context) error {
	base := c.Target()
	if base == "" {
		return fmt.Errorf("ollama: not activated")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: daemon at %s is not answering: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("ollama: daemon at %s returned %s", base, resp.Status)
	}
	return nil
}

// resolve picks the daemon URL: an operator-declared one, else what the
// detector found. A refusal names the probe so an enabled unit on a host
// with no Ollama reports why.
func (c *ollamaCapability) resolve(cfg service.Config) (string, error) {
	if explicit := firstNonEmpty(setting(cfg, "url"), cfg.Env["OLLAMA_HOST"]); explicit != "" {
		return explicit, nil
	}
	detect := c.Detect
	if detect == nil {
		detect = agent.DetectOllama
	}
	bt := detect()
	if !bt.Available || bt.URL == "" {
		return "", fmt.Errorf("ollama: no Ollama daemon detected (tried %s) — start it, or set `settings.url`", firstNonEmpty(bt.URL, defaultOllamaURL))
	}
	return bt.URL, nil
}

// defaultOllamaURL is Ollama's own default endpoint — the target the warm
// plane and the LAN inference proxy fall back to when the proxy unit is off
// and nothing else named one.
const defaultOllamaURL = "http://127.0.0.1:11434"

// ollamaPoolCapability runs the registry-push watcher.
type ollamaPoolCapability struct {
	// Ollama is the per-host proxy unit's capability. The pool rides on it:
	// the same Counter feeds the capacity probe and the push payload, so the
	// two can never report different numbers.
	Ollama *ollamaCapability
	// Config, AgentName, Cloudbox and AccessToken are the host's identity
	// and credentials — resolved by the daemon, never declared.
	AgentName   func() string
	Cloudbox    func() string
	AccessToken func() string
	// Warm supplies the considerate warm-serving signal folded into each
	// push; LANEndpoint the direct same-LAN inference URL. Both nil-safe:
	// the corresponding unit being off simply drops the field.
	Warm        func() (func() (int64, bool), bool)
	LANEndpoint func() string
	// Cluster layers the intra-home cluster and shard descriptors onto the
	// push so cloudbox can size-gate routing.
	Cluster func(base ollama.ClusterSource) ollama.ClusterSource

	run goroutineRunner

	mu    sync.Mutex
	bound *ollama.Service
}

func (c *ollamaPoolCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Ollama == nil {
		return fmt.Errorf("ollama pool: no ollama proxy capability wired")
	}
	svc := c.Ollama.Service()
	target := c.Ollama.Target()
	if svc == nil || target == "" {
		return fmt.Errorf("ollama pool: the ollama proxy unit is not active — enable it first (the pool is an extension of the per-host proxy)")
	}
	cb := resolveFn(c.Cloudbox)
	if cb == "" {
		return fmt.Errorf("ollama pool: no cloudbox URL — the pool needs a paired host")
	}
	ocfg := ollama.Config{
		AgentName:   resolveFn(c.AgentName),
		Version:     agent.ReadBuildInfo().Short(),
		OllamaURL:   target,
		CloudboxURL: cb,
		AccessToken: resolveFn(c.AccessToken),
		// Capacity is the Service, not the Counter: Service composes the v2
		// CapacityReport (counter snapshot + the watcher's /api/ps cache)
		// so the push and the /_pool/capacity probe emit the same shape.
		Capacity: svc,
	}
	// Bashy's own Ollama engine keeps a separate model store from the host
	// daemon: register its models too, so a bashy-managed pull shows up in
	// the cloud registry. Dropped when it duplicates the primary URL.
	if bu := ollama.BashyOllamaURL(); bu != "" {
		ocfg.ExtraOllamaURLs = []string{bu}
	}
	if c.Warm != nil {
		if fn, ok := c.Warm(); ok {
			ocfg.WarmStatus = fn
		}
	}
	if c.LANEndpoint != nil {
		ocfg.LANEndpoint = c.LANEndpoint()
	}
	if c.Cluster != nil {
		ocfg.Cluster = c.Cluster(nil)
	}
	w, err := ollama.New(ocfg)
	if err != nil {
		return fmt.Errorf("ollama pool: watcher init: %w", err)
	}
	// Close the loop so Service.Status() reports last-push state through one
	// accessor.
	svc.SetWatcher(w)
	c.mu.Lock()
	c.bound = svc
	c.mu.Unlock()
	c.run.start(ctx, func(runCtx context.Context) error {
		if err := w.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
	slog.Info("ollama pool: publishing this host's model registry", "cloudbox", cb, "target", target)
	return nil
}

// Configure rebuilds the watcher: its config (cloudbox URL, LAN endpoint,
// cluster source) is read once at construction.
func (c *ollamaPoolCapability) Configure(ctx context.Context, cfg service.Config) error {
	return c.Activate(ctx, cfg)
}

func (c *ollamaPoolCapability) Deactivate(ctx context.Context) error {
	c.run.stop(ctx)
	c.mu.Lock()
	bound := c.bound
	c.bound = nil
	c.mu.Unlock()
	if bound != nil {
		bound.SetWatcher(nil)
	}
	return nil
}

// Health reports the push loop's liveness AND that the proxy it rides is
// still the same one.
//
// The second half matters because the two are separate units: if the proxy
// cycles (its daemon moved, or its own health failed), it builds a fresh
// in-flight counter, and a pool that kept pushing through the old one would
// report capacity for a daemon nothing is proxying any more. Reporting it
// unhealthy makes the reconciler cycle the pool onto the live counter —
// which is what `requires:` means at runtime.
func (c *ollamaPoolCapability) Health(context.Context) error {
	if err := c.run.health("ollama pool"); err != nil {
		return err
	}
	c.mu.Lock()
	bound := c.bound
	c.mu.Unlock()
	if c.Ollama == nil {
		return fmt.Errorf("ollama pool: no ollama proxy capability wired")
	}
	live := c.Ollama.Service()
	if live == nil {
		return fmt.Errorf("ollama pool: the ollama proxy unit is no longer active")
	}
	if bound != live {
		return fmt.Errorf("ollama pool: the ollama proxy was cycled — rebinding to its live capacity counter")
	}
	return nil
}

// resolveFn calls a nil-safe string closure.
func resolveFn(f func() string) string {
	if f == nil {
		return ""
	}
	return f()
}
