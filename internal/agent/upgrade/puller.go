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

	"golang.org/x/mod/semver"

	"github.com/qiangli/outpost/internal/agent"
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
	// Safety net: the PULL path must NEVER move a host to a version that isn't
	// strictly NEWER than what it runs. cloudbox's fleet "target" is just its
	// latest-release record, which can lag or point at an older tag; a puller
	// that blindly applied it would DOWNGRADE the host — exactly how a
	// freshly-fixed Windows host got repeatedly yanked back to an old build,
	// undoing the fix minutes after each manual upgrade. (The operator-driven
	// PUSH path can still force a deliberate rollback; only this automatic pull
	// is guarded.)
	if ok, reason := shouldDeclinePulledTarget(agent.ReadBuildInfo(), env); ok {
		slog.Info("upgrade puller: declining fleet target — not strictly newer (no auto-downgrade)",
			"current", agent.ReadBuildInfo().Version, "target", env.ReleaseID, "reason", reason)
		return
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

// declinePulledTarget reports whether the automatic pull must DECLINE the
// offered target because it is not strictly NEWER than the running binary.
// target is the fleet target's release_id (a vX.Y.Z semver tag):
//   - target not valid semver          → decline (apply only clean release tags)
//   - running binary is untagged (dev) → decline (never let the fleet silently
//     replace a dev/-dirty build — e.g. a hand-built host — with a release)
//   - target <= current                → decline (a downgrade or lateral move)
//
// Only a strictly-newer, validly-tagged target is applied. This is the robust
// guard: even a stale or wrong cloudbox target can no longer walk a host
// backward via the silent pull.
//
// Deprecated: use shouldDeclinePulledTarget with full BuildInfo + Envelope for
// time-aware checks. This wrapper is retained for backward compatibility and
// delegates to shouldDeclinePulledTarget with an empty BuildTime.
func declinePulledTarget(target string) bool {
	ok, _ := shouldDeclinePulledTarget(agent.ReadBuildInfo(), Envelope{ReleaseID: target})
	return ok
}

// shouldDeclinePulledTarget reports whether the automatic pull must DECLINE
// the offered envelope because it is not strictly NEWER than the running
// binary. It considers three guards:
//
//  1. Semver validity and ordering: both versions must be valid semver and
//     target must be strictly greater than current via semver.Compare.
//
//  2. Prerelease base guard: if the current build is a prerelease (-dev/-rc)
//     whose stripped base version is >= the target's stripped base version,
//     the target is declined even when semver.Compare says it is newer.
//     Stripping means removing a trailing "-dev" suffix (e.g. v0.32.0-dev →
//     v0.32.0, v1.0.0-rc.12-dev → v1.0.0-rc.12). This prevents a dev build
//     that is newer than the release (by build time) from being downgraded
//     to the older release code.
//
//  3. Build-time guard: when the envelope carries an optional `build_time`
//     (RFC3339) and the current binary has a VCSTime, decline if the target
//     time is not strictly after the current time. This is the authoritative
//     wall-clock ordering that semver alone cannot provide.
//
// It returns (decline, reason). A non-empty reason is suitable for logging.
func shouldDeclinePulledTarget(cur agent.BuildInfo, target Envelope) (bool, string) {
	if !semver.IsValid(target.ReleaseID) {
		return true, "target not valid semver"
	}
	if !semver.IsValid(cur.Version) {
		return true, "current version not valid semver"
	}
	// 3. Build-time guard (authoritative when both times present).
	targetNewerByTime := false
	if target.BuildTime != "" && cur.VCSTime != "" {
		tTarget, err1 := parseBuildTime(target.BuildTime)
		tCur, err2 := parseBuildTime(cur.VCSTime)
		if err1 == nil && err2 == nil {
			if !tTarget.After(tCur) {
				return true, "target build_time not newer than current vcs_time"
			}
			targetNewerByTime = true
		}
	}
	// 2. Prerelease base guard — skipped when time proves target is newer.
	if !targetNewerByTime && semver.Prerelease(cur.Version) != "" {
		curBase := stripDevSuffix(cur.Version)
		targetBase := stripDevSuffix(target.ReleaseID)
		if semver.IsValid(curBase) && semver.IsValid(targetBase) {
			if semver.Compare(curBase, targetBase) >= 0 {
				return true, "current prerelease base >= target base"
			}
		}
	}
	// 1. Semver ordering.
	if semver.Compare(target.ReleaseID, cur.Version) <= 0 {
		return true, "target semver not strictly newer than current"
	}
	return false, ""
}

// ShouldDeclineCandidate is the shared never-downgrade guard for the
// operator-driven paths: the CLI (`--from` / `--local` / `--direct`,
// checked after probing the candidate) and the cloudbox push
// (Worker.Apply). It wraps shouldDeclinePulledTarget — same semver +
// prerelease-base + build/commit-time ordering — but is lenient about
// versions it cannot order: when either side is not valid semver there
// is no proven downgrade, so it allows the candidate (probe, sha256,
// and platform checks still apply downstream).
//
// Force never bypasses this: `--force` / Force=true keeps its current
// meaning (reinstall the same commit) and does NOT allow downgrades. A
// deliberate downgrade stays possible only through the rollback path
// (Worker.Rollback, i.e. `outpost rollback`), which restores the
// retained `<binary>.previous` instead of applying an older release.
//
// The returned reason names both versions and the rollback command, so
// it is suitable as a user-facing refusal message verbatim.
func ShouldDeclineCandidate(cur agent.BuildInfo, releaseID, buildTime string) (bool, string) {
	if !semver.IsValid(releaseID) || !semver.IsValid(cur.Version) {
		return false, ""
	}
	decline, reason := shouldDeclinePulledTarget(cur, Envelope{ReleaseID: releaseID, BuildTime: buildTime})
	if !decline {
		return false, ""
	}
	return true, fmt.Sprintf("refusing to replace running %s with candidate %s (%s) — deliberate rollback only via `outpost rollback`",
		cur.Version, releaseID, reason)
}

func stripDevSuffix(v string) string {
	if idx := strings.Index(v, "-dev"); idx != -1 {
		return v[:idx]
	}
	return v
}

func parseBuildTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Fallback: Go's vcs.time sometimes lacks timezone? Try without.
	return time.Parse("2006-01-02T15:04:05Z07:00", s)
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
