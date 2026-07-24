package overlaykey

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// DefaultInterval is how often the refresher checks overlay health.
//
// The failure it repairs is rare (a cloudbox deploy) but leaves the node
// unusable for cluster networking until fixed, so a minute of extra
// downtime is worse than a cheap poll. The check itself is one exec into a
// container when healthy.
const DefaultInterval = 60 * time.Second

// ExecFunc runs a command inside the runtime container.
type ExecFunc func(ctx context.Context, args ...string) ([]byte, error)

// Refresher keeps this host registered on the overlay.
//
// It exists because overlay credentials used to be a boot-time-only
// affair: pairing and reattach handed them over, and reattach runs when
// the daemon starts. Anything that invalidated the tailnet registration
// afterwards — a cloudbox deploy resetting Headscale being the common
// case — left the node off the pod network until a human restarted the
// daemon. Nothing was broken that could not be re-issued; the node simply
// never asked.
type Refresher struct {
	Client   *Client
	Exec     ExecFunc
	PodCIDR  string
	Interval time.Duration
	Log      *slog.Logger
}

func (r *Refresher) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Refresher) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return DefaultInterval
}

// tailscaleStatus is the sliver of `tailscale status --json` we read.
type tailscaleStatus struct {
	BackendState string `json:"BackendState"`
}

// Healthy reports whether tailscaled considers itself logged in and running.
//
// A failure to ASK is deliberately not "unhealthy": the container may be
// gone, tailscale may not be installed, or the overlay may simply be off on
// this host. Treating "could not determine" as "broken" would have the
// refresher mint keys and run `tailscale up` against hosts that never
// wanted an overlay — acting on the absence of evidence.
func (r *Refresher) Healthy(ctx context.Context) (bool, error) {
	out, err := r.Exec(ctx, "tailscale", "status", "--json")
	if err != nil {
		return false, err
	}
	var st tailscaleStatus
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		return false, jerr
	}
	return strings.EqualFold(st.BackendState, "Running"), nil
}

// Heal fetches a fresh single-use key and re-registers this node.
func (r *Refresher) Heal(ctx context.Context) error {
	creds, err := r.Client.Fetch(ctx)
	if err != nil {
		return err
	}
	podCIDR := strings.TrimSpace(creds.PodCIDR)
	if podCIDR == "" {
		podCIDR = strings.TrimSpace(r.PodCIDR)
	}

	args := []string{
		"tailscale", "up",
		"--login-server=" + creds.LoginServer,
		"--authkey=" + creds.AuthKey,
		"--reset",
		"--accept-routes",
	}
	if podCIDR != "" {
		// Re-advertise on every heal: a control-plane reset drops the
		// approved routes with everything else, and a node that rejoins
		// without re-advertising is on the tailnet but carries no pod
		// traffic — healthy-looking and useless.
		args = append(args, "--advertise-routes="+podCIDR)
	}
	if out, err := r.Exec(ctx, args...); err != nil {
		return errors.New("tailscale up failed: " + strings.TrimSpace(string(out)))
	}
	r.logger().Info("overlay: re-registered with a fresh key", "pod_cidr", podCIDR)
	return nil
}

// Run polls until ctx is done, healing when the overlay is not Running.
func (r *Refresher) Run(ctx context.Context) error {
	if r.Client == nil || r.Exec == nil {
		return errors.New("overlaykey: Refresher needs Client and Exec")
	}
	tick := time.NewTicker(r.interval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			healthy, err := r.Healthy(ctx)
			if err != nil {
				// Cannot tell — say nothing and try later. See Healthy.
				continue
			}
			if healthy {
				continue
			}
			if err := r.Heal(ctx); err != nil {
				switch {
				case errors.Is(err, ErrOverlayDisabled):
					// Nothing to heal toward; stop rather than poll a
					// feature that is off.
					r.logger().Info("overlay: cloudbox reports the overlay is disabled; refresher stopping")
					return nil
				case errors.Is(err, ErrThrottled):
					// Expected under a fleet-wide reset; the next tick
					// will be past cloudbox's floor.
				default:
					r.logger().Warn("overlay: re-registration failed", "err", err)
				}
			}
		}
	}
}
