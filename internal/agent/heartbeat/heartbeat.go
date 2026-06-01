// Package heartbeat owns the outpost → cloudbox active liveness push
// (Layer-5 defense). Distinct from the cloudbox-driven /apps poll
// (which writes Host.LastSeenAt only when the matrix tunnel is up):
// this path goes over HTTPS to /api/v1/host/heartbeat, so we get an
// independent signal that the outpost process itself is healthy even
// when the matrix tunnel is degraded or restarting.
//
// One Worker per outpost, started under the same errgroup as the
// tunnel by cmd/outpost/main.go. Failures back off exponentially
// (10s..5min, capped). 401 is fatal — it means the bearer was
// revoked cloudbox-side and we should stop trying until the operator
// re-pairs (logged once, then sleep until ctx cancels).
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Config is everything Worker needs at construction. The fields are
// snapshots taken at start time; if any change (pairing rotated, etc.)
// the daemon self-restarts and a fresh Worker picks the new values up.
type Config struct {
	// CloudboxBase is the HTTPS base URL of cloudbox (e.g.
	// "https://ai.dhnt.io"). Empty disables the worker.
	CloudboxBase string

	// AccessToken is the per-outpost bearer (FileConfig.AccessToken).
	// Empty disables the worker.
	AccessToken string

	// AgentName is the host name registered with cloudbox.
	AgentName string

	// BuildCommit / BuildVersion are stamped onto each heartbeat for
	// post-incident correlation ("which version was this outpost
	// running when it went dark?").
	BuildCommit  string
	BuildVersion string

	// ClusterStatusFn returns "up", "down", or "off" each tick.
	// Optional — nil means we don't report cluster state.
	ClusterStatusFn func() string

	// SelfcheckStatusFn returns "ok", "warn", or "fail". Optional.
	SelfcheckStatusFn func() string

	// Interval is the gap between successful heartbeats. Default 5m.
	Interval time.Duration

	// Client lets tests substitute an *http.Client. Nil = default.
	Client *http.Client
}

// Worker is the lifecycle handle. Run blocks until ctx is done.
type Worker struct {
	cfg Config
}

func New(cfg Config) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Worker{cfg: cfg}
}

// Run drives the heartbeat loop. Returns nil on ctx.Done().
// Logs but never propagates a network/HTTP error — heartbeat is a
// signal, not a hard dependency.
func (w *Worker) Run(ctx context.Context) error {
	if w.cfg.CloudboxBase == "" || w.cfg.AccessToken == "" || w.cfg.AgentName == "" {
		slog.Info("heartbeat: disabled (unpaired or missing cloudbox URL)")
		<-ctx.Done()
		return nil
	}
	// First heartbeat after a short jitter so newly-paired outposts
	// don't all fire at the same instant.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	backoff := 10 * time.Second
	const backoffMax = 5 * time.Minute
	authRevokedQuietUntil := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		next := w.cfg.Interval
		if !authRevokedQuietUntil.IsZero() && time.Now().Before(authRevokedQuietUntil) {
			// We saw a 401 recently; keep quiet on the net but the
			// loop must still tick so a daemon restart picks up
			// the new credential. We just skip the POST.
			timer.Reset(next)
			continue
		}

		err := w.postOne(ctx)
		switch {
		case err == nil:
			backoff = 10 * time.Second
			timer.Reset(next)
		case errors.Is(err, errAuthRevoked):
			// Don't spam the log every interval; quiet for 1h and
			// then retry once (in case the operator re-paired).
			authRevokedQuietUntil = time.Now().Add(1 * time.Hour)
			slog.Warn("heartbeat: cloudbox returned 401; pausing pushes for 1h",
				"agent", w.cfg.AgentName)
			timer.Reset(next)
		default:
			slog.Warn("heartbeat: push failed, will retry",
				"err", err, "backoff", backoff)
			timer.Reset(backoff)
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
		}
	}
}

var errAuthRevoked = errors.New("heartbeat: 401 from cloudbox; pairing was pulled")

func (w *Worker) postOne(ctx context.Context) error {
	body := map[string]any{
		"agent_name":    w.cfg.AgentName,
		"build_commit":  w.cfg.BuildCommit,
		"build_version": w.cfg.BuildVersion,
	}
	if w.cfg.ClusterStatusFn != nil {
		body["cluster_status"] = w.cfg.ClusterStatusFn()
	}
	if w.cfg.SelfcheckStatusFn != nil {
		body["selfcheck_status"] = w.cfg.SelfcheckStatusFn()
	}
	buf, _ := json.Marshal(body)
	url := strings.TrimRight(w.cfg.CloudboxBase, "/") + "/api/v1/host/heartbeat"

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errAuthRevoked
	default:
		return fmt.Errorf("heartbeat: cloudbox returned %d: %s",
			resp.StatusCode, bytes.TrimSpace(respBody))
	}
}
