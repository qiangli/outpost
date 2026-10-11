package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	actrunner "github.com/qiangli/yoke/external/actrunner"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/telemetry"
)

// The act_runner CI executor as an in-process capability (unit
// `provider: internal`, `owner: outpost`, `capability: outpost.actrunner`).
//
// Everything else outpost used to launch — Zot, SeaweedFS, Kopia, the
// Headlamp port-forward — is a plain `provider: process` unit now: a
// loopback server with a port and a data dir is fully expressible as
// `exec` + `ports` + `expose`, so the monitor owns the child and outpost
// owns nothing but the publication. act_runner is the one that is not,
// and for a reason worth stating:
//
//   - it has a PROVISIONING step. `act_runner register` is a one-shot
//     handshake against the forge (instance + token + labels) that must
//     succeed, and be retried, before the daemon can run at all; a unit
//     declaration has no pre-start hook to put it in (see
//     docs/settings.md, "Contract notes for the unit seeds").
//   - two of its inputs are RESOLVED on this host, not declared: the forge
//     URL defaults to the loom unit's own declared listener, and the
//     sandbox executor's DOCKER_HOST comes from `bashy podman machine
//     inspect`.
//   - its job environment carries the operator's VAULT secrets, so a CI
//     deploy step inherits GITHUB_TOKEN without a per-repo secret.
//
// The declaration still decides everything decidable: `enabled` is the only
// switch (there is no actrunner_* key in agent.json any more), and the
// instance / token / labels / sandbox / docker_host / data_dir / version
// come from `settings` and `secrets`.
type actrunnerCapability struct {
	// Forge resolves the loopback URL of a local git-forge unit, used when
	// the declaration names no instance. Set from localForgeURL.
	Forge func(unit string) string
	// VaultEnv loads the operator's vault secrets as KEY=VALUE entries.
	VaultEnv func(ctx context.Context) []string
	// DockerHost resolves the sandbox executor's container runtime.
	DockerHost func(ctx context.Context, explicit string) string

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	runErr  error
	started bool
}

// actrunnerSettings is the declaration as this capability reads it.
type actrunnerSettings struct {
	instance     string
	token        string
	labels       string
	name         string
	version      string
	dataDir      string
	sandbox      bool
	sandboxImage string
	dockerHost   string
	forgeUnit    string
}

// readActrunnerSettings projects a unit's settings and resolved env onto the
// runner's inputs. The token is read from the env FIRST: a unit's `secrets`
// are vault references resolved into env, and a token pasted into `settings`
// would sit in the registry in plaintext.
func readActrunnerSettings(cfg service.Config) actrunnerSettings {
	s := actrunnerSettings{
		instance:     setting(cfg, "instance"),
		token:        firstNonEmpty(cfg.Env["ACTRUNNER_TOKEN"], cfg.Env["GITEA_RUNNER_REGISTRATION_TOKEN"], setting(cfg, "token")),
		labels:       setting(cfg, "labels"),
		name:         setting(cfg, "name"),
		version:      setting(cfg, "version"),
		dataDir:      setting(cfg, "data_dir"),
		sandbox:      settingBool(cfg, "sandbox"),
		sandboxImage: setting(cfg, "sandbox_image"),
		dockerHost:   firstNonEmpty(setting(cfg, "docker_host"), cfg.Env["DOCKER_HOST"]),
		forgeUnit:    setting(cfg, "forge_unit"),
	}
	if s.labels == "" {
		s.labels = actrunner.DefaultLabels
	}
	if s.forgeUnit == "" {
		s.forgeUnit = "loom"
	}
	if s.dataDir == "" {
		s.dataDir = defaultExternalDataDir("act_runner")
	}
	return s
}

// Activate registers (if needed) and runs the daemon under a supervising
// loop. It returns as soon as the loop is started — the unit is Starting at
// that point and the internal provider records it as Active; a later failure
// of the loop surfaces through Health.
func (c *actrunnerCapability) Activate(ctx context.Context, cfg service.Config) error {
	s := readActrunnerSettings(cfg)
	if s.instance == "" && c.Forge != nil {
		s.instance = c.Forge(s.forgeUnit)
	}
	if s.instance == "" {
		return fmt.Errorf("act_runner: no forge instance — set `settings.instance` on the %s unit or enable the %s unit", cfg.Unit, s.forgeUnit)
	}
	if !actrunner.Registered(s.dataDir) && s.token == "" {
		return fmt.Errorf("act_runner: not registered and no registration token — add a `secrets` entry for ACTRUNNER_TOKEN (vault) or `settings.token`")
	}

	// Idempotent: a re-Activate replaces the running loop rather than
	// leaking a second daemon onto the same data dir.
	if err := c.Deactivate(ctx); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.mu.Lock()
	c.cancel, c.done, c.runErr, c.started = cancel, done, nil, true
	c.mu.Unlock()

	go func() {
		defer close(done)
		err := c.supervise(runCtx, s)
		c.mu.Lock()
		c.runErr = err
		c.mu.Unlock()
	}()
	return nil
}

// Configure: the registration handshake is baked into the data dir at
// register time, so a changed declaration is applied by cycling the unit.
func (c *actrunnerCapability) Configure(context.Context, service.Config) error {
	return service.ErrRestartRequired
}

// Deactivate stops the daemon and waits for it to go, so a cycle cannot
// overlap two children on one data dir.
func (c *actrunnerCapability) Deactivate(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel, c.done, c.started = nil, nil, false
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		return fmt.Errorf("act_runner: daemon did not exit within 15s")
	case <-ctx.Done():
	}
	return nil
}

// Health reports the supervising loop's liveness. The loop retries on its
// own, so an error here means it gave up (or ended) — which the reconciler
// turns into a withdrawn, non-active unit instead of a silent no-op.
func (c *actrunnerCapability) Health(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return fmt.Errorf("act_runner: not activated")
	}
	if c.runErr != nil {
		return c.runErr
	}
	select {
	case <-c.done:
		return fmt.Errorf("act_runner: supervising loop exited")
	default:
		return nil
	}
}

// supervise is the register + daemon loop. A build-only node reaches its
// forge over a mesh forward that may not be up yet, so the first
// register/poll can fail — it retries with bounded backoff instead of
// stranding the runner until the next daemon restart.
func (c *actrunnerCapability) supervise(ctx context.Context, s actrunnerSettings) error {
	backoff := 5 * time.Second
	const maxBackoff = 2 * time.Minute
	for ctx.Err() == nil {
		if !actrunner.Registered(s.dataDir) {
			if s.token == "" {
				return fmt.Errorf("act_runner: not registered and no registration token")
			}
			if err := actrunner.Register(ctx, actrunner.RegisterOptions{
				Version: s.version, DataDir: s.dataDir, Instance: s.instance,
				Token: s.token, Name: s.name, Labels: s.labels,
				Sandbox: s.sandbox, SandboxImage: s.sandboxImage,
			}); err != nil {
				slog.Warn("act_runner: registration failed; retrying", "err", err, "backoff", backoff)
				if !sleepCtx(ctx, backoff) {
					return nil
				}
				backoff = minDur(backoff*2, maxBackoff)
				continue
			}
			slog.Info("act_runner: registered", "instance", s.instance, "labels", s.labels)
			backoff = 5 * time.Second
		}
		env := telemetry.ChildEnv("act_runner")
		if s.sandbox {
			// The sandbox config may be missing when the runner was
			// registered before sandbox mode was asked for (Register writes
			// it, but that path is skipped once .runner exists).
			if err := actrunner.EnsureSandboxConfig(s.dataDir); err != nil {
				slog.Warn("act_runner sandbox: writing config failed", "err", err)
			}
			if dh := c.resolveDockerHost(ctx, s.dockerHost); dh != "" {
				env = append(env, "DOCKER_HOST="+dh)
				slog.Info("act_runner sandbox: DOCKER_HOST resolved", "docker_host", dh)
			} else {
				slog.Warn("act_runner sandbox: no DOCKER_HOST — runs-on:sandbox jobs will fail (set settings.docker_host or start bashy podman); host-lane jobs still run")
			}
		}
		// The operator's vault secrets, so CI jobs inherit GITHUB_TOKEN and
		// friends WITHOUT a per-repo secret — the token stays in the vault.
		if c.VaultEnv != nil {
			if vault := c.VaultEnv(ctx); len(vault) > 0 {
				env = append(env, vault...)
				slog.Info("act_runner: loaded vault secrets into job env", "count", len(vault))
			}
		}
		slog.Info("act_runner: starting CI daemon", "instance", s.instance)
		err := actrunner.Daemon(ctx, s.version, s.dataDir, env...)
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("act_runner: daemon exited; restarting", "err", err, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = minDur(backoff*2, maxBackoff)
	}
	return nil
}

func (c *actrunnerCapability) resolveDockerHost(ctx context.Context, explicit string) string {
	if c.DockerHost == nil {
		return explicit
	}
	return c.DockerHost(ctx, explicit)
}

// defaultExternalDataDir is where a wrapped tool keeps its state when the
// declaration names no `settings.data_dir`: the same per-tool subdir of
// outpost's cache dir the pre-unit boot code used, so an existing host keeps
// its registry, repo or runner identity across this change.
func defaultExternalDataDir(tool string) string {
	if cd, _ := conf.ResolveCacheDir(); cd != "" {
		return filepath.Join(cd, tool)
	}
	return tool
}

// resolveActrunnerDockerHost returns the DOCKER_HOST the tier-3 sandbox
// executor dials to reach the container runtime. An explicit setting wins;
// otherwise it asks bashy podman for its host-side machine socket. Returns
// "" when it can't resolve — host-lane (runs-on: host) jobs still run; only
// runs-on: sandbox needs a container runtime.
func resolveActrunnerDockerHost(ctx context.Context, explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	bin, err := bashyResolver.Path(ctx)
	if err != nil {
		slog.Warn("act_runner sandbox: cannot resolve bashy to find the podman socket; set settings.docker_host", "err", err)
		return ""
	}
	out, err := exec.CommandContext(ctx, bin, "podman", "machine", "inspect",
		"--format", "{{.ConnectionInfo.PodmanSocket.Path}}").Output()
	if err != nil {
		slog.Warn("act_runner sandbox: `bashy podman machine inspect` failed; set settings.docker_host or start the podman machine", "err", err)
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return ""
	}
	if strings.Contains(p, "://") {
		return p
	}
	return "unix://" + p
}

// loadVaultSecretsEnv resolves the operator's cloudbox-vault secrets into
// KEY=VALUE env entries by running `bashy secrets env` (which reads the vault
// via the file-based secrets token, so it works from the daemon). Best-effort:
// any error yields no entries. `bashy secrets env` emits shell
// `export KEY='value'` lines (single-quoted); we unquote them.
func loadVaultSecretsEnv(ctx context.Context) []string {
	bin, err := bashyResolver.Path(ctx)
	if err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, bin, "secrets", "env").Output()
	if err != nil {
		slog.Warn("act_runner: could not load vault secrets (bashy secrets env) — CI deploy steps needing GITHUB_TOKEN may fail", "err", err)
		return nil
	}
	var env []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key, val := line[:eq], line[eq+1:]
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			val = strings.ReplaceAll(val[1:len(val)-1], `'\''`, `'`) // shell single-quote unescape
		}
		env = append(env, key+"="+val)
	}
	return env
}

// setting reads one declared setting, trimmed.
func setting(cfg service.Config, key string) string {
	return strings.TrimSpace(cfg.Settings[key])
}

// settingBool reads a declared boolean. YAML settings are strings, so the
// accepted spellings are the ones an operator actually writes.
func settingBool(cfg service.Config, key string) bool {
	switch strings.ToLower(setting(cfg, key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sleepCtx sleeps for d unless ctx ends first; it reports whether the sleep
// completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// minDur returns the smaller of two durations.
func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
