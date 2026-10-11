package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/filebrowser/filebrowser/v2/fbembed"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// The embedded File Browser as an in-process capability (unit
// `provider: internal`, `owner: outpost`, `capability: outpost.files`).
//
// Why a capability and not a `provider: process` unit: the handler is
// compiled into this binary (there is no command to exec), the listener is
// bound by this process, and the app route it publishes needs facets a
// declaration cannot express — `index_path: /`, and the per-app gate the
// matrix tunnel applies. What the DECLARATION owns is everything decidable:
// `enabled` is the only switch, `settings.scope` is the confined root and
// `settings.allow_write` flips read-only ⇄ read-write.
//
// `settings.allow_write` keeps the property the old `files_allow_write` key
// had: it is only reachable from the host (the loopback admin plane, the CLI
// or `bashy service set`), never from the cloud-facing surface — which is
// what makes "read-only by default" a guarantee rather than a default.
type filesCapability struct {
	// Apps is the live tunnel registry the mount publishes into.
	Apps *agent.AppRegistry
	// SigningKey persists (and mints on first use) the File Browser session
	// key, so sessions survive a daemon restart. It is outpost's secret, not
	// the declaration's — a unit never carries an inline secret.
	SigningKey func() ([]byte, error)

	srv httpServerRunner

	mu      sync.Mutex
	scope   string
	write   bool
	applied bool
}

// filesSettings is the declaration as this capability reads it.
type filesSettings struct {
	scope      string
	allowWrite bool
	port       int
}

func readFilesSettings(cfg service.Config) filesSettings {
	s := filesSettings{
		scope:      setting(cfg, "scope"),
		allowWrite: settingBool(cfg, "allow_write"),
		// 0 = an ephemeral loopback port, which is what the mount used
		// before: the tunnel reaches it through the app registry, so the
		// number is nobody's business. A declaration that pins one is
		// honoured (and then checked for conflicts by fleet).
		port: configPort(cfg, "", 0),
	}
	if s.scope == "" {
		if home, err := os.UserHomeDir(); err == nil {
			s.scope = home
		}
	}
	return s
}

// Activate builds the handler, binds the loopback listener and registers the
// app route. A bind failure is returned (not logged and swallowed) so the
// unit reports failed with the reason instead of looking healthy while
// serving nothing.
func (c *filesCapability) Activate(ctx context.Context, cfg service.Config) error {
	if c.Apps == nil {
		return fmt.Errorf("files: no app registry on this host")
	}
	s := readFilesSettings(cfg)
	var key []byte
	if c.SigningKey != nil {
		k, err := c.SigningKey()
		if err != nil {
			// Non-fatal, same as before: an ephemeral key means sessions do
			// not survive a restart, which is better than no file browser.
			slog.Warn("files: session signing key", "err", err)
		}
		key = k
	}
	// The embed is stateless (no DB): scope + write mode come from the
	// declaration and UI prefs live in the browser's localStorage.
	h, closer, err := fbembed.New(fbembed.Options{
		Scope: s.scope, AllowWrite: s.allowWrite, SigningKey: key,
	})
	if err != nil {
		return fmt.Errorf("files: init: %w", err)
	}
	if err := c.deactivate(ctx); err != nil {
		_ = closer()
		return err
	}
	port, err := c.srv.start(loopbackAddr(s.port), h, closer)
	if err != nil {
		return fmt.Errorf("files: listen: %w", err)
	}
	if err := c.Apps.RegisterFromConfig(conf.AppConfig{
		Name: agent.BuiltinFiles, Scheme: "http", Host: "127.0.0.1", Port: port,
		RequireLogin: true, IndexPath: "/", Enabled: true,
		// No /api/users LAN-fence needed: the embed is stateless, perm and
		// scope come from the declaration, and every write to the in-memory
		// user no-ops — a client cannot enable write or change scope.
	}); err != nil {
		_ = c.srv.stop(ctx)
		return fmt.Errorf("files: register app: %w", err)
	}
	c.mu.Lock()
	c.scope, c.write, c.applied = s.scope, s.allowWrite, true
	c.mu.Unlock()
	slog.Info("files: serving the embedded File Browser", "scope", s.scope, "write", s.allowWrite, "port", port)
	return nil
}

// Configure: scope and write mode are baked into the handler at construction,
// so a changed declaration is applied by cycling the unit.
func (c *filesCapability) Configure(context.Context, service.Config) error {
	return service.ErrRestartRequired
}

// Deactivate withdraws the app route and stops the listener.
func (c *filesCapability) Deactivate(ctx context.Context) error { return c.deactivate(ctx) }

func (c *filesCapability) deactivate(ctx context.Context) error {
	c.mu.Lock()
	applied := c.applied
	c.applied = false
	c.mu.Unlock()
	if applied && c.Apps != nil {
		c.Apps.Unregister(agent.BuiltinFiles)
	}
	return c.srv.stop(ctx)
}

// Health reports the listener's liveness.
func (c *filesCapability) Health(context.Context) error { return c.srv.health("files") }

// loopbackAddr is the listen address for a capability-owned loopback
// listener. Port 0 asks the kernel for an ephemeral one.
func loopbackAddr(port int) string {
	if port <= 0 {
		return "127.0.0.1:0"
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}
