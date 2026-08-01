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

// unreadableLogEvery throttles the "cannot probe" warning: log the first
// occurrence, then every Nth, so a permanently unreachable container
// leaves a trail without flooding.
const unreadableLogEvery = 30

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

	// IgnoreCredentialPodCIDR, when true, makes Heal ignore a fetched
	// Credentials.PodCIDR entirely and advertise only r.PodCIDR (which may
	// be empty, in which case Heal advertises nothing).
	//
	// Set this for a peer-hosted-plane worker (PeerFlannel). Under that
	// mode the pod CIDR comes from flannel reading Node.spec.podCIDR, which
	// the PEER's k3s controller-manager allocates
	// (docs/adr-peer-dks-pod-network.md, Option A). The fetched
	// Credentials.PodCIDR is cloudbox's cloudbox-cluster carve and has no
	// authority over the peer's pod network — honoring it on every Heal
	// would hand the container a CIDR to advertise routes for and
	// reintroduce exactly the competing allocator that ADR removed. Left
	// false (the default), Heal keeps its original behavior byte-for-byte:
	// a fetched Credentials.PodCIDR wins, falling back to r.PodCIDR.
	IgnoreCredentialPodCIDR bool

	// ControlURL, when set, overrides the login server returned by
	// cloudbox. It exists because the public overlay URL is fronted by
	// Cloudflare, which strips the Upgrade header on the ts2021 control
	// POST — so the client must instead reach Headscale over the
	// frp-tunnelled loopback endpoint (http://127.0.0.1:<port>/overlay/
	// headscale). Heal execs inside the runtime container, where that
	// visitor is bound, so the loopback URL resolves.
	ControlURL string
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
	BackendState string      `json:"BackendState"`
	Self         *peerStatus `json:"Self"`
	Health       []string    `json:"Health"`
}

// peerStatus is the sliver of a node's own status we read. InNetworkMap is the
// load-bearing field: it is false whenever this node is not currently in a
// netmap it successfully polled — exactly the stranded state a Running-only
// check cannot see.
type peerStatus struct {
	InNetworkMap bool `json:"InNetworkMap"`
}

// Healthy reports whether tailscaled is logged in AND actually in the network
// map.
//
// BackendState=="Running" alone is NOT sufficient: after a cloudbox deploy
// resets Headscale's node DB, tailscaled keeps reporting "Running" from cached
// state while every /machine/map poll gets a 404 "node not found" — the node
// is stranded (logged out at the control plane) yet self-reports Running. A
// Running-only check then leaves it stranded forever: the refresher never
// heals what looks healthy, so a deploy takes the whole fleet's overlay down
// until each node is restarted by hand. Self.InNetworkMap is the honest
// signal — false in exactly that stranded state — so requiring BOTH is what
// makes the refresher actually re-register after a deploy.
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
	if !strings.EqualFold(st.BackendState, "Running") {
		return false, nil
	}
	if hasCoordinationServerFailure(st.Health) {
		return false, nil
	}
	return st.Self != nil && st.Self.InNetworkMap, nil
}

func hasCoordinationServerFailure(health []string) bool {
	for _, msg := range health {
		if strings.Contains(strings.ToLower(msg), "unable to connect to the tailscale coordination server") {
			return true
		}
	}
	return false
}

// Heal fetches a fresh single-use key and re-registers this node.
func (r *Refresher) Heal(ctx context.Context) error {
	creds, err := r.Client.Fetch(ctx)
	if err != nil {
		return err
	}
	var podCIDR string
	if r.IgnoreCredentialPodCIDR {
		// See IgnoreCredentialPodCIDR: never honor cloudbox's cluster carve
		// on a peer-hosted plane, even though this Heal call just fetched
		// one — the peer's flannel controller-manager is the only allocator
		// with authority here.
		podCIDR = strings.TrimSpace(r.PodCIDR)
	} else {
		podCIDR = strings.TrimSpace(creds.PodCIDR)
		if podCIDR == "" {
			podCIDR = strings.TrimSpace(r.PodCIDR)
		}
	}

	loginServer := strings.TrimSpace(creds.LoginServer)
	if r.ControlURL != "" {
		// Reach Headscale over the Cloudflare-free tunnelled loopback,
		// not the public URL cloudbox reported. See ControlURL.
		loginServer = r.ControlURL
	}

	args := []string{
		"tailscale", "up",
		"--login-server=" + loginServer,
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

	// `tailscale up` exits 0 while leaving the node logged out — observed
	// against a control plane that could not complete registration. So the
	// exit code is not evidence that we rejoined; ask what actually
	// happened. Without this, a refresher that heals nothing reports a
	// successful re-registration on every tick.
	healthy, herr := r.Healthy(ctx)
	if herr != nil {
		return errors.New("tailscale up returned 0 but state could not be read: " + herr.Error())
	}
	if !healthy {
		return errors.New("tailscale up returned 0 but the node is still not registered")
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
	// unreadable counts consecutive ticks where the state could not be
	// probed at all, so the warning can be throttled rather than dropped.
	var unreadable int
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			healthy, err := r.Healthy(ctx)
			if err != nil {
				// Cannot tell — do not act on it (see Healthy), but do
				// not be silent about it either: a refresher failing to
				// probe on every tick otherwise looks exactly like one
				// with nothing to do. Throttled so a permanently
				// unreachable container cannot flood the log.
				unreadable++
				if unreadable == 1 || unreadable%unreadableLogEvery == 0 {
					r.logger().Warn("overlay: cannot read tailscale state; not acting",
						"consecutive", unreadable, "err", err)
				}
				continue
			}
			unreadable = 0
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
