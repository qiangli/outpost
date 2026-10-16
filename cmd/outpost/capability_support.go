package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// Shared helpers for the in-process capabilities (capability_*.go).
//
// Everything here reads a DECLARATION and nothing else: a capability's whole
// configuration surface is `settings`, `env` and `ports` on its unit, which
// is what makes `bashy service set <unit> --set k=v` the single way to
// configure it and `enabled` the single way to switch it on.

// settingList reads a declared list setting. Units are YAML strings, so a
// list is written comma- or whitespace-separated — the two spellings an
// operator actually types for `allowed_images` or `peers`.
func settingList(cfg service.Config, key string) []string {
	raw := setting(cfg, key)
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// settingBoolDefault reads a declared boolean with an explicit default, for
// the settings whose absence means "on" (require_login, pool participation).
func settingBoolDefault(cfg service.Config, key string, def bool) bool {
	switch strings.ToLower(setting(cfg, key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// settingInt reads a declared integer, def when absent or unparseable.
func settingInt(cfg service.Config, key string, def int64) int64 {
	raw := setting(cfg, key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// settingUint reads a declared unsigned integer, def when absent.
func settingUint(cfg service.Config, key string, def uint64) uint64 {
	raw := setting(cfg, key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// settingFloat reads a declared float, def when absent or unparseable.
func settingFloat(cfg service.Config, key string, def float64) float64 {
	raw := setting(cfg, key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}

// configPort resolves the port a capability should listen on: the named
// listener when the declaration has one, else the primary, else def.
//
// This is what puts an in-process listener under fleet's port-conflict check
// — the declaration is the only place the number is written, so two units
// cannot quietly claim the same port the way two agent.json keys could.
func configPort(cfg service.Config, name string, def int) int {
	var primary fleet.ServicePort
	found := false
	for _, p := range cfg.Ports {
		if p.Name == name && p.Port > 0 {
			return p.Port
		}
		if !found && p.Port > 0 {
			primary, found = p, true
		}
	}
	if found {
		return primary.Port
	}
	return def
}

// httpServerRunner is the lifecycle shared by every capability that owns an
// HTTP listener (the File Browser mount, the LAN inference proxy): bind on
// Activate so a port clash is reported as an activation refusal rather than
// buried in a goroutine, serve until Deactivate, and report the serving
// state through Health.
type httpServerRunner struct {
	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	port int
	// closers run after the server is shut down, in order.
	closers []func() error
	errc    chan error
	runErr  error
}

// start binds addr and serves h. It returns the bound port.
func (r *httpServerRunner) start(addr string, h http.Handler, closers ...func() error) (int, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		for _, c := range closers {
			if c != nil {
				_ = c()
			}
		}
		return 0, err
	}
	srv := &http.Server{Handler: h}
	errc := make(chan error, 1)
	r.mu.Lock()
	r.srv, r.ln, r.closers, r.errc, r.runErr = srv, ln, closers, errc, nil
	r.port = ln.Addr().(*net.TCPAddr).Port
	port := r.port
	r.mu.Unlock()
	go func() {
		err := srv.Serve(ln)
		if err == http.ErrServerClosed {
			err = nil
		}
		errc <- err
		close(errc)
	}()
	return port, nil
}

// stop shuts the server down and runs the closers. Idempotent.
func (r *httpServerRunner) stop(ctx context.Context) error {
	r.mu.Lock()
	srv, closers := r.srv, r.closers
	r.srv, r.ln, r.closers, r.errc, r.port = nil, nil, nil, nil, 0
	r.mu.Unlock()
	if srv == nil {
		return nil
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(sctx)
	for _, c := range closers {
		if c == nil {
			continue
		}
		if cerr := c(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// health reports whether the listener is still serving. A server that
// returned an error has stopped serving, and no monitor can see that for an
// in-process capability — so reporting it here is what gets the unit marked
// failed and its app route withdrawn.
func (r *httpServerRunner) health(what string) error {
	r.mu.Lock()
	srv, errc := r.srv, r.errc
	runErr := r.runErr
	r.mu.Unlock()
	if srv == nil {
		return fmt.Errorf("%s: not activated", what)
	}
	if runErr != nil {
		return runErr
	}
	select {
	case err, ok := <-errc:
		if !ok {
			return fmt.Errorf("%s: listener stopped serving", what)
		}
		r.mu.Lock()
		if err == nil {
			err = fmt.Errorf("%s: listener stopped serving", what)
		}
		r.runErr = err
		r.mu.Unlock()
		return err
	default:
		return nil
	}
}

// boundPort is the port the listener actually took, 0 when inactive.
func (r *httpServerRunner) boundPort() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.port
}

// goroutineRunner is the lifecycle shared by the capabilities whose work is
// a long-running goroutine rather than a listener (the pool watcher, the
// warm supervisor, the shard manager, the sandbox prewarmer).
type goroutineRunner struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	runErr  error
	started bool
}

// start runs fn under a context derived from ctx, replacing any previous run.
func (r *goroutineRunner) start(ctx context.Context, fn func(context.Context) error) {
	r.stop(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.mu.Lock()
	r.cancel, r.done, r.runErr, r.started = cancel, done, nil, true
	r.mu.Unlock()
	go func() {
		defer close(done)
		err := fn(runCtx)
		r.mu.Lock()
		r.runErr = err
		r.mu.Unlock()
	}()
}

// stop cancels the goroutine and waits briefly for it. Idempotent.
func (r *goroutineRunner) stop(ctx context.Context) {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done, r.started = nil, nil, false
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
	}
}

// health reports whether the goroutine is still running.
func (r *goroutineRunner) health(what string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return fmt.Errorf("%s: not activated", what)
	}
	if r.runErr != nil {
		return r.runErr
	}
	select {
	case <-r.done:
		return fmt.Errorf("%s: worker exited", what)
	default:
		return nil
	}
}

func (r *goroutineRunner) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

// absentError marks a refusal that means "the backend this capability fronts
// is not on this host (yet)" rather than "the capability is broken". A
// default-on unit must stay quiet in that state: reporting it failed would
// walk it through the monitor's start limit on every host without the
// backend, which is what boot-time detection avoided before units existed.
type absentError struct{ msg string }

func (e *absentError) Error() string { return e.msg }

func newAbsent(format string, a ...any) error {
	return &absentError{msg: fmt.Sprintf(format, a...)}
}

func isAbsent(err error) bool {
	var a *absentError
	return errors.As(err, &a)
}

// idleState is the non-failing "unavailable: <what was probed>" state shared
// by the capabilities whose backend can be missing. Activate records the
// reason and returns nil (nothing is published); Health re-activates with the
// remembered declaration so the capability comes up once the backend appears.
type idleState struct {
	mu     sync.Mutex
	reason string
	cfg    service.Config
}

// set records the reason, logging once per distinct reason.
func (s *idleState) set(what string, cfg service.Config, reason string) {
	s.mu.Lock()
	changed := s.reason != reason
	s.reason, s.cfg = reason, cfg
	s.mu.Unlock()
	if changed {
		slog.Info(what+": unavailable, will re-probe", "reason", reason)
	}
}

func (s *idleState) clear() {
	s.mu.Lock()
	s.reason = ""
	s.mu.Unlock()
}

// pending returns the remembered declaration when the capability is idle.
func (s *idleState) pending() (service.Config, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.reason != ""
}

// Unavailable is the reason the backend is absent, "" when not idle.
func (s *idleState) Unavailable() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}
