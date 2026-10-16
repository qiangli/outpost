package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent/ollama"
)

// The same-LAN direct-inference listener as an in-process capability (unit
// `provider: internal`, `owner: outpost`, `capability: outpost.lan-inference`).
//
// It binds a LAN-reachable port that reverse-proxies the OpenAI `/v1` and
// Ollama `/api` surfaces to this host's local inference server, so a
// same-LAN caller reaches the LLM directly instead of going out through the
// cloudbox relay. This is a LAN-TRUST endpoint: it is NOT authenticated per
// request, which is why it is off by default and why turning it on is the
// operator stating that their LAN is trusted. Untrusted or org networks
// leave the unit disabled and use the Bearer-authed cloudbox gateway.
//
// The port lives in the DECLARATION (`ports: [{port: 11436, bind: lan}]`),
// which is the material improvement over the old `lan_inference_port` key:
// fleet now checks it against every other enabled unit's listeners, so two
// services can no longer be configured onto one port and discover it at
// boot. `settings.target` overrides the upstream for a host whose inference
// server is not the local Ollama.
type lanInferenceCapability struct {
	// Ollama resolves the local inference daemon to proxy to.
	Ollama func() string

	srv httpServerRunner
}

// DefaultLANInferencePort is the port the listener takes when the
// declaration names none. Deliberately distinct from Ollama's own 11434:
// this is a separate, LAN-bound surface in front of it.
const DefaultLANInferencePort = 11436

// Endpoint is the URL to advertise to cloudbox so it can hand this host's
// direct-LAN inference URL to same-LAN callers, "" while the unit is not
// serving or when the host has no private LAN IPv4.
func (c *lanInferenceCapability) Endpoint() string {
	if c == nil {
		return ""
	}
	port := c.srv.boundPort()
	if port == 0 {
		return ""
	}
	return ollama.LANEndpoint(port)
}

func (c *lanInferenceCapability) Activate(ctx context.Context, cfg service.Config) error {
	target := setting(cfg, "target")
	if target == "" && c.Ollama != nil {
		target = c.Ollama()
	}
	if target == "" {
		target = ollama.DefaultURL
	}
	tu, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("lan inference: bad target URL %q: %w", target, err)
	}
	rp := httputil.NewSingleHostReverseProxy(tu)
	mux := http.NewServeMux()
	mux.Handle("/v1/", rp)
	mux.Handle("/api/", rp)
	if err := c.srv.stop(ctx); err != nil {
		return err
	}
	port := configPort(cfg, "", DefaultLANInferencePort)
	// All interfaces: reaching this host from the LAN is the entire point.
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	if _, err := c.srv.start(addr, mux); err != nil {
		return fmt.Errorf("lan inference: listen on %s: %w", addr, err)
	}
	slog.Info("lan inference: serving direct same-LAN inference (LAN-trust, no per-request auth)", "addr", addr, "target", target)
	return nil
}

// Configure rebinds: both the port and the target are fixed at listen time.
func (c *lanInferenceCapability) Configure(context.Context, service.Config) error {
	return service.ErrRestartRequired
}

func (c *lanInferenceCapability) Deactivate(ctx context.Context) error { return c.srv.stop(ctx) }

func (c *lanInferenceCapability) Health(context.Context) error {
	return c.srv.health("lan inference")
}
