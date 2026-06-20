package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/jitter"
)

// Puller is the pull half of fleet upgrade. On startup and periodically
// thereafter it asks cloudbox for the current fleet target — the latest
// published release's artifact for this host's platform — and hands any
// envelope it gets to the Worker.
//
// The push half (cloudbox POSTing /admin/upgrade during a rollout)
// reaches hosts that are ONLINE when a release fans out. The Puller is
// what lets a host that was asleep or offline at that moment catch up:
// on its next poll after the tunnel reconnects, it reconciles against
// the latest release and self-upgrades if behind.
//
// It adds no new trust or policy. It reuses Worker.Apply, whose
// update_mode gate (a "manual"/"never" host no-ops without Force) plus
// same-commit / replay checks make a poll a cheap no-op whenever the
// host is already current or has opted out of automatic upgrades. The
// envelope it builds is advisory (Force=false), exactly like the
// release-webhook-driven push.

const (
	defaultPullInterval = 10 * time.Minute
	pullRequestTimeout  = 20 * time.Second
)

// PullerConfig configures and runs a Puller. CloudboxBase, AccessToken,
// Platform, and Worker are required; the rest take defaults.
type PullerConfig struct {
	// CloudboxBase is the cloudbox origin (scheme+host), e.g.
	// https://ai.dhnt.io — the same base the ollama watcher / backup
	// pusher use. The /api/v1/fleet/target path is appended.
	CloudboxBase string
	// AccessToken is the per-outpost bearer presented to cloudbox.
	AccessToken string
	// Platform selects the artifact, in <goos>_<goarch> form matching
	// the release webhook's artifact map keys (e.g. "windows_amd64").
	Platform string
	// Worker applies any envelope the poll returns.
	Worker *Worker
	// Interval between polls. <=0 → defaultPullInterval.
	Interval time.Duration
	// InitialDelay before the first poll (lets the tunnel settle after
	// a fresh start or wake). <0 → a random delay in [0, Interval) so a
	// fleet rebooted together doesn't poll in lockstep; 0 polls
	// immediately (used by tests).
	InitialDelay time.Duration
	// HTTPClient is the client used for the target GET. nil →
	// http.DefaultClient.
	HTTPClient *http.Client
}

// Run blocks until ctx is canceled, polling the fleet target on the
// configured cadence. It returns nil on cancel. A misconfigured Puller
// (unpaired host, no worker) logs once and blocks — it never errors the
// errgroup it runs under. Note that a successful apply restarts the
// daemon, which cancels ctx and ends this goroutine.
func (p PullerConfig) Run(ctx context.Context) error {
	if p.Worker == nil || strings.TrimSpace(p.CloudboxBase) == "" || strings.TrimSpace(p.AccessToken) == "" {
		slog.Info("upgrade puller: not configured (unpaired or no worker); pull-trigger disabled")
		<-ctx.Done()
		return nil
	}
	interval := p.Interval
	if interval <= 0 {
		interval = defaultPullInterval
	}
	delay := p.InitialDelay
	if delay < 0 {
		// Default: smear the first poll across a full interval window so a
		// fleet rebooted together (exactly the post-upgrade case) does not
		// poll cloudbox in lockstep. Explicit positive = exact; 0 = now.
		delay = jitter.Full(interval)
	}

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(delay):
	}

	for {
		p.checkOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		// Jitter each tick over [interval/2, interval) so steady-state polls
		// stay de-correlated across the fleet while keeping a sane min gap.
		case <-time.After(interval/2 + jitter.Full(interval/2)):
		}
	}
}

// checkOnce fetches the fleet target and, when one is offered, hands it
// to the Worker. All failures are logged, not fatal — the next tick
// retries.
func (p PullerConfig) checkOnce(ctx context.Context) {
	env, ok, err := p.fetchTarget(ctx)
	if err != nil {
		slog.Warn("upgrade puller: fetch target failed", "err", err)
		return
	}
	if !ok {
		return // 204 — no release on file, or none for this platform
	}
	res := p.Worker.Apply(ctx, env)
	switch res.Status {
	case StatusAccepted, StatusPendingManual:
		slog.Info("upgrade puller: fleet target applied",
			"status", res.Status, "release_id", env.ReleaseID, "commit", env.Commit)
	default:
		// same_commit / replay / disabled / min_from / in_flight — the
		// expected steady-state no-ops.
		slog.Debug("upgrade puller: target poll no-op",
			"status", res.Status, "release_id", env.ReleaseID, "commit", env.Commit)
	}
}

// fetchTarget GETs the fleet target for this host's platform. Returns
// (env, true, nil) on 200, (Envelope{}, false, nil) on 204 (nothing to
// do), and an error on anything else.
func (p PullerConfig) fetchTarget(ctx context.Context) (Envelope, bool, error) {
	base := strings.TrimRight(p.CloudboxBase, "/")
	target := base + "/api/v1/fleet/target?platform=" + p.Platform
	cctx, cancel := context.WithTimeout(ctx, pullRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return Envelope{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+p.AccessToken)

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Envelope{}, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return Envelope{}, false, nil
	case http.StatusOK:
		var env Envelope
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&env); err != nil {
			return Envelope{}, false, fmt.Errorf("decode target: %w", err)
		}
		return env, true, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return Envelope{}, false, fmt.Errorf("cloudbox returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}
