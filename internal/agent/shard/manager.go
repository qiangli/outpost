package shard

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent/brain"
)

// readinessDeadline bounds how long a leader waits for its ring to become
// serveable before tearing the stuck shard down — freeing the serving port a
// never-joining worker would otherwise zombie at "503 Loading model".
const readinessDeadline = 3 * time.Minute

// ShardPeer identifies a shard-ring participant on the mesh: a hostname label
// plus its libp2p peer id (what the forwarder dials).
type ShardPeer struct {
	Host   string
	PeerID string
}

// PeerDiscoverer yields the reachable same-LAN owner peers eligible as shard
// workers. The real implementation wraps the peer-plane (same-LAN/tier filter)
// + cloudbox peer/connect (peer-id resolution); tests inject a fake.
type PeerDiscoverer interface {
	SameLANPeers(ctx context.Context) ([]ShardPeer, error)
}

// ManagerConfig configures the shard manager.
type ManagerConfig struct {
	Self      ShardPeer      // this host (label + its own libp2p peer id)
	Forwarder Forwarder      // the mesh forwarder (the data plane)
	Peers     PeerDiscoverer // same-LAN owner-peer source
	Interval  time.Duration  // discover cadence (0 → 30s)
	Logger    *slog.Logger
	Bins      ServeBins // this node's prima binaries (server + worker)
	// LocalLoad yields this node's local models (with sizes) + its model-memory
	// budget; when set, the discover loop auto-triggers a shard for a too-big
	// model (MaybeShard). nil → no auto-trigger.
	LocalLoad func() ([]LocalModel, uint64)
	APIPort   int // OpenAI port for a leader-served shard (0 → 11434)
	// Provision ensures the model (+ engine binaries) are present locally,
	// fetching them with no human staging, and returns the GGUF path prima loads.
	// nil → identity (model name used as-is; for tests + already-staged hosts).
	Provision func(ctx context.Context, modelName string) (string, error)
	// Refiner, when set, lets the pooled LLM (the brain) refine the leader
	// election. nil → the deterministic bootstrap (most-VRAM) stands.
	Refiner brain.Refiner
	// LogDir, when set, is where each rank's prima stdout+stderr is captured
	// (<LogDir>/prima-rank<N>.log) — the exit reason when a shard process dies.
	LogDir string
}

// Manager keeps a current candidate shard Ring up to date: it periodically
// discovers the reachable same-LAN owner peers and assembles a launch-ready
// ring. It does NOT form a shard by itself — standing the ring up is gated on a
// too-big model (the auto-trigger, v1d); the manager just keeps the ring ready.
type Manager struct {
	self      ShardPeer
	fwd       Forwarder
	peers     PeerDiscoverer
	interval  time.Duration
	log       *slog.Logger
	bins      ServeBins
	localLoad func() ([]LocalModel, uint64)
	apiPort   int
	// onForm is the action taken to stand up a rank (default Form); injectable
	// so the orchestration control plane can be tested without launching.
	onForm func(context.Context, *Ring, int, ServeConfig) error
	// orchestrate stands up a full shard with this node as leader (default
	// Orchestrate); injectable so the auto-trigger decision is testable.
	orchestrate func(context.Context, string, int, []string) error
	// decide chooses whether to shard + which node leads (default DecideShard,
	// deterministic most-VRAM); the LLM "self-think" path swaps in here.
	decide Decider
	// gather collects candidate capacities for the election (default
	// gatherViaPing, over the mesh); injectable so the trigger is unit-testable.
	gather func(ctx context.Context, modelBytes, selfBudget uint64) ([]NodeCapacity, map[string]ShardPeer)
	// provision fetches the model (+ binaries) and returns the GGUF path (default
	// identity); self-provisioning is what removes human staging.
	provision func(ctx context.Context, modelName string) (string, error)
	// refiner is the pooled-LLM hook for the brain (nil → bootstrap stands).
	refiner brain.Refiner
	// logDir is where each rank's prima output is captured (empty → discarded).
	logDir string

	mu          sync.Mutex
	ring        *Ring
	active      *Session
	activeModel string
	lastExit    string // last prima exit (model + error) — surfaced in status for remote diagnosis
}

// NewManager builds a shard manager. Defaults: 30s discover interval, the
// default slog logger.
func NewManager(cfg ManagerConfig) *Manager {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	apiPort := cfg.APIPort
	if apiPort == 0 {
		apiPort = 11434
	}
	m := &Manager{
		self:      cfg.Self,
		fwd:       cfg.Forwarder,
		peers:     cfg.Peers,
		interval:  interval,
		log:       log,
		bins:      cfg.Bins,
		localLoad: cfg.LocalLoad,
		apiPort:   apiPort,
	}
	m.onForm = m.Form
	m.orchestrate = m.Orchestrate
	m.decide = DecideShard
	m.gather = m.gatherViaPing
	m.provision = cfg.Provision
	if m.provision == nil {
		m.provision = func(_ context.Context, name string) (string, error) { return name, nil }
	}
	m.refiner = cfg.Refiner
	m.logDir = cfg.LogDir
	return m
}

// Run refreshes the candidate ring immediately, then on every interval, until
// ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	// Serve the shard-control endpoint so a leader can drive this node to form
	// its rank (best-effort — discovery still runs if it can't bind).
	if m.fwd != nil {
		if cleanup, err := m.ServeControl(); err != nil {
			m.log.Warn("shard: control endpoint unavailable", "err", err)
		} else {
			defer cleanup()
		}
	}
	m.refresh(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.refresh(ctx)
		}
	}
}

func (m *Manager) refresh(ctx context.Context) {
	ring, err := m.buildRing(ctx)
	if err != nil {
		m.log.Debug("shard: discover failed", "err", err)
		return
	}
	m.mu.Lock()
	prev := m.ring
	m.ring = ring
	m.mu.Unlock()
	if ring != nil && (prev == nil || len(prev.Members) != len(ring.Members)) {
		m.log.Info("shard: candidate ring", "members", len(ring.Members), "leader", m.self.Host)
	}
	m.maybeAutoShard(ctx)
}

// maybeAutoShard fires the auto-trigger with this node's local models + budget,
// if a LocalLoad source is wired (best-effort; logs and moves on).
func (m *Manager) maybeAutoShard(ctx context.Context) {
	if m.localLoad == nil {
		return
	}
	models, budget := m.localLoad()
	if err := m.MaybeShard(ctx, models, budget, m.apiPort); err != nil {
		m.log.Debug("shard: auto-trigger failed", "err", err)
	}
}

// buildRing discovers same-LAN owner peers and assembles a candidate Ring: this
// host as rank 0 (the leader placeholder — v1d re-picks by VRAM) plus the peers
// in stable host order. Returns nil when there are no peers (a one-node "ring"
// can't shard).
func (m *Manager) buildRing(ctx context.Context) (*Ring, error) {
	peers, err := m.peers.SameLANPeers(ctx)
	if err != nil {
		return nil, err
	}
	if len(peers) == 0 {
		return nil, nil
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Host < peers[j].Host })

	members := make([]Member, 0, len(peers)+1)
	members = append(members, Member{Rank: 0, Host: m.self.Host, PeerID: m.self.PeerID})
	for i, p := range peers {
		members = append(members, Member{Rank: i + 1, Host: p.Host, PeerID: p.PeerID})
	}
	return &Ring{Members: members}, nil
}

// Ring returns a snapshot of the current candidate ring (nil if there are no
// same-LAN peers to shard with).
func (m *Manager) Ring() *Ring {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ring
}

// Form launches THIS node's part of a shard for the given ring + serve config
// (the caller — the trigger — decides ring/rank/model/when). The leader serves
// the model's OpenAI endpoint; workers serve their layer span. Recording the
// served model lets the pool advertise it (ActiveModel). Forming again replaces
// the previous shard.
func (m *Manager) Form(ctx context.Context, ring *Ring, myRank int, sc ServeConfig) error {
	gguf, err := m.provision(ctx, sc.Model)
	if err != nil {
		return fmt.Errorf("shard: provision %q: %w", sc.Model, err)
	}
	plan, err := ring.PlanFor(myRank)
	if err != nil {
		return err
	}
	launchSC := sc
	launchSC.Model = gguf // prima loads the resolved GGUF path
	lc := plan.LaunchConfigFor(launchSC)
	logw := m.primaLog(myRank)
	if logw != nil {
		lc.LogWriter = logw
	}
	sess, err := Start(ctx, m.fwd, plan, lc)
	if err != nil {
		if logw != nil {
			logw.Close()
		}
		return err
	}
	m.mu.Lock()
	prev := m.active
	m.active = sess
	m.activeModel = sc.Model // advertise the NAME, not the GGUF path
	m.mu.Unlock()
	if prev != nil {
		prev.Stop()
	}
	m.log.Info("shard: formed", "model", sc.Model, "rank", myRank, "members", len(ring.Members))
	// Watch the process: when prima exits (clean or crash) clear the active state
	// so a re-trigger re-forms instead of no-opping on a dead shard, and surface
	// the exit (the captured prima-rank<N>.log says why).
	go m.watchSession(sess, sc.Model, logw)
	return nil
}

// primaLog opens this rank's prima stdout+stderr sink, truncated. Returns nil
// when no log dir is configured or the file can't be opened — prima then runs
// with its output discarded (the prior behavior).
func (m *Manager) primaLog(rank int) io.WriteCloser {
	if m.logDir == "" {
		return nil
	}
	if err := os.MkdirAll(m.logDir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(m.logDir, fmt.Sprintf("prima-rank%d.log", rank)),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// watchSession blocks until the shard's prima process exits, then clears the
// active state (if this is still the live session) and closes its log. A dead
// shard that left activeModel set is what made a re-trigger a no-op.
func (m *Manager) watchSession(sess *Session, model string, logw io.Closer) {
	err := sess.Wait()
	if logw != nil {
		logw.Close()
	}
	// Unwind the mesh wiring even when prima exited on its own. The ring uses
	// FIXED forward ports (shard-signal/shard-data), so a crash that leaves the
	// listeners bound makes the NEXT form fail with "address already in use".
	// Stop is idempotent (already-dead process → Kill no-ops, cleanup runs once).
	sess.Stop()
	m.mu.Lock()
	cleared := m.active == sess
	if cleared {
		m.active = nil
		m.activeModel = ""
	}
	if err != nil {
		m.lastExit = fmt.Sprintf("%s: %v", model, err)
	} else {
		m.lastExit = fmt.Sprintf("%s: exited cleanly", model)
	}
	m.mu.Unlock()
	if cleared {
		m.log.Warn("shard: prima exited — cleared active shard", "model", model, "err", err)
	}
}

// LastExit returns a description of the most recent prima exit on this node
// (model + error), or "" if none. It's surfaced in the status report so a
// worker-rank crash is visible over the mesh — no ssh into the box needed.
func (m *Manager) LastExit() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastExit
}

// RecentPrimaLogs returns the tail of every captured prima-rank log on this
// node: each <logDir>/prima-rank<N>.log prefixed with a "==== <name> ===="
// header and its last maxLines lines. Returns "" when no log dir is configured
// or none exist. This is the self-diagnosis surface — the exit reason a crashed
// rank left behind, readable over the mesh (via /log) with no ssh into the box.
func (m *Manager) RecentPrimaLogs(maxLines int) string {
	if m.logDir == "" {
		return ""
	}
	files, err := filepath.Glob(filepath.Join(m.logDir, "prima-rank*.log"))
	if err != nil || len(files) == 0 {
		return ""
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if maxLines > 0 && len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		fmt.Fprintf(&b, "==== %s ====\n", filepath.Base(f))
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// monitorRing is the leader's readiness watchdog (spawned by Orchestrate after
// rank 0 forms). It polls the served OpenAI /health every 5s until it returns
// 200 (ring ready → serving) or readinessDeadline passes. On timeout it pulls
// each worker's prima log over the mesh — so the worker's error lands in THIS
// (leader) node's log — and tears the stuck shard down (Stop frees the ports +
// clears state), instead of zombie-ing the serving port forever.
func (m *Manager) monitorRing(ring *Ring, model string, apiPort int) {
	healthURL := fmt.Sprintf("http://%s:%d/health", loopback, apiPort)
	deadline := time.Now().Add(readinessDeadline)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		<-t.C
		if primaHealthy(healthURL) {
			m.log.Info("shard: ring ready — serving", "model", model, "members", len(ring.Members))
			return
		}
		if time.Now().After(deadline) {
			break
		}
	}
	m.log.Warn("shard: ring not ready, tearing down stuck shard", "model", model, "after", readinessDeadline)
	for _, member := range ring.Members {
		if member.Rank == 0 {
			continue // the leader itself
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		logText, err := m.PeerLog(ctx, ShardPeer{Host: member.Host, PeerID: member.PeerID})
		cancel()
		if err != nil {
			logText = "<unavailable: " + err.Error() + ">"
		}
		m.log.Warn("shard: worker log", "host", member.Host, "rank", member.Rank, "log", logText)
	}
	m.Stop()
}

// primaHealthy reports whether the leader's served OpenAI endpoint answers 200
// (the ring formed and the model finished loading).
func primaHealthy(healthURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// noteExit records a worker-rank form failure in the status diagnostic, so a
// leader's async /form can surface why a worker didn't stand up — visible over
// the mesh via /status, no ssh into the box.
func (m *Manager) noteExit(model, detail string) {
	m.mu.Lock()
	m.lastExit = fmt.Sprintf("%s: %s", model, detail)
	m.mu.Unlock()
}

// Stop tears down the active shard on this node (if any).
func (m *Manager) Stop() {
	m.mu.Lock()
	sess := m.active
	m.active = nil
	m.activeModel = ""
	m.mu.Unlock()
	if sess != nil {
		sess.Stop()
	}
}

// ActiveModel returns the model this node is currently serving via a shard, or
// "" if none — the name the pool advertises so cloudbox routes requests for it
// to this (leader) node.
func (m *Manager) ActiveModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeModel
}
