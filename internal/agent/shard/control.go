package shard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ControlService is the mesh-forwarder service name the shard-control endpoint
// is exposed under: a leader reaches a worker's control endpoint over the mesh
// to tell it to stand up its rank.
const ControlService = "shard-ctl"

// ServeBins are this node's Prima binaries. Resolved per host — paths differ by
// OS/install, so a worker always uses its OWN, never the leader's.
type ServeBins struct {
	ServerBin string // prima llama-server (leader, rank 0)
	WorkerBin string // prima llama-cli (workers)
}

// FormRequest is the leader→worker control message: stand up your rank in this
// ring for this model. Binaries are NOT carried — the worker resolves its own.
type FormRequest struct {
	Ring    Ring     `json:"ring"`
	MyRank  int      `json:"my_rank"`
	Model   string   `json:"model"`
	APIPort int      `json:"api_port"`
	Extra   []string `json:"extra,omitempty"`
}

// serveConfig builds a ServeConfig from the request + this node's own binaries.
func (m *Manager) serveConfig(req FormRequest) ServeConfig {
	return ServeConfig{
		Model:     req.Model,
		ServerBin: m.bins.ServerBin,
		WorkerBin: m.bins.WorkerBin,
		APIPort:   req.APIPort,
		Extra:     req.Extra,
	}
}

// ServeControl runs the shard-control HTTP handler on a fresh loopback listener
// and exposes it over the mesh as ControlService, so a leader can drive this
// node to form its rank. The returned cleanup unexposes + shuts it down.
func (m *Manager) ServeControl() (func(), error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/form", func(w http.ResponseWriter, r *http.Request) {
		var req FormRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Stand the rank up in the BACKGROUND on a detached context, then 202.
		// Forming self-provisions + launches prima, which can take long enough that
		// holding this mesh-forwarded POST open until it returns risks a "connection
		// reset by peer". The leader no longer blocks on the worker's full form; a
		// worker failure is surfaced via the worker's last_exit (read over /status).
		sc := m.serveConfig(req)
		ring := req.Ring
		rank := req.MyRank
		model := req.Model
		go func() {
			if err := m.onForm(context.Background(), &ring, rank, sc); err != nil {
				m.log.Warn("shard: worker form failed", "rank", rank, "model", model, "err", err)
				m.noteExit(model, "worker form failed: "+err.Error())
			}
		}()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.LocalStatus())
	})
	mux.HandleFunc("/log", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, m.RecentPrimaLogs(200))
	})
	// Provision asks this node to (idempotently) land its signed prima engine
	// binaries. Long-running (a first-time fetch) → backgrounded on a detached
	// context with an immediate 202, like /form: the leader's bounded poll of
	// /status observes the binaries landing instead of holding this
	// mesh-forwarded POST open. A signature failure errors in the background
	// and the binaries stay absent — the readiness gate stays closed.
	mux.HandleFunc("/provision", func(w http.ResponseWriter, _ *http.Request) {
		go func() {
			if err := m.ensureBins(context.Background()); err != nil {
				m.log.Warn("shard: peer-asked prima provisioning failed", "err", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/lead", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model   string `json:"model"`
			APIPort int    `json:"api_port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// This node becomes the leader: orchestrate a shard for the model
		// (self-provision + drive the workers). Long-running → background it.
		go func() {
			if err := m.orchestrate(context.Background(), req.Model, req.APIPort, nil); err != nil {
				m.log.Warn("shard: lead orchestrate failed", "model", req.Model, "err", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
	})
	ln, err := net.Listen("tcp", loopback+":0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	m.fwd.Expose(ControlService, ln.Addr().String())
	return func() {
		m.fwd.Unexpose(ControlService)
		_ = srv.Close()
	}, nil
}

// Orchestrate forms a shard for the model across the current ring with THIS node
// as leader (rank 0): it tells every worker (over the mesh shard-control) to
// stand up its rank, then forms its own. The caller (the trigger) decides when
// and which model. Fail-fast: a worker that won't form aborts the whole form.
func (m *Manager) Orchestrate(ctx context.Context, model string, apiPort int, extra []string) error {
	// The mesh resolve/forward to a peer flakes transiently (a momentary flap can
	// drop the candidate ring or reset the form POST). Wait briefly for the ring
	// if it's momentarily empty rather than failing the whole trigger.
	var ring *Ring
	for attempt := 0; attempt < 5; attempt++ {
		if ring = m.Ring(); ring != nil {
			break
		}
		if !sleepCtx(ctx, 6*time.Second) {
			return ctx.Err()
		}
	}
	if ring == nil {
		return fmt.Errorf("shard: no candidate ring (no same-LAN peers)")
	}
	// Fresh-host round: ask every candidate peer to land its signed prima
	// engine, then wait boundedly for the binaries to appear — otherwise the
	// readiness gate below drops every fresh peer (nothing provisions the
	// bins outside a form) and the first form can never pass. Best-effort:
	// the gate still drops whoever stays unready.
	m.provisionPeers(ctx, ring)
	// Readiness-gate the ring: a same-LAN peer that can't answer the shard
	// readiness ping (sharding off, no worker binary, wrong platform) drops out
	// and the ranks re-number, instead of aborting the form or hanging the prima
	// ring on a rank that never comes up. Same gate the election uses.
	ring = m.readyRing(ctx, ring)
	if len(ring.Members) < 2 {
		return fmt.Errorf("shard: same-LAN peers found but none are shard-ready (no worker answered the readiness ping)")
	}
	for _, member := range ring.Members {
		if member.Rank == 0 {
			continue // self = leader; formed last, below
		}
		req := FormRequest{Ring: *ring, MyRank: member.Rank, Model: model, APIPort: apiPort, Extra: extra}
		// Retry a worker through a transient mesh failure (resolve flake / forward
		// reset) instead of aborting the whole form on the first hiccup.
		var werr error
		for attempt := 0; attempt < 5; attempt++ {
			if werr = m.tellWorker(ctx, member, req); werr == nil {
				break
			}
			m.log.Warn("shard: tellWorker transient failure, retrying", "host", member.Host, "attempt", attempt+1, "err", werr)
			if !sleepCtx(ctx, time.Duration(2*(attempt+1))*time.Second) {
				return ctx.Err()
			}
		}
		if werr != nil {
			return fmt.Errorf("shard: form worker %s (rank %d): %w", member.Host, member.Rank, werr)
		}
	}
	err := m.onForm(ctx, ring, 0, ServeConfig{
		Model: model, ServerBin: m.bins.ServerBin, WorkerBin: m.bins.WorkerBin, APIPort: apiPort, Extra: extra,
	})
	if err == nil {
		// Leader formed: watch the ring become serveable, and if no worker joins
		// before readinessDeadline, capture each worker's prima log here and tear
		// the stuck shard down — so a never-joining worker can't zombie the port.
		go m.monitorRing(ring, model, apiPort)
	}
	return err
}

// readyRing rebuilds base to include only peers that are actually shard-ready:
// they answer the readiness ping AND report a worker binary on disk. A same-LAN
// peer that's reachable but can't run a rank — no provisioned prima worker
// binary on disk — is dropped rather than aborting the form or hanging the
// prima ring on a rank that never comes up. Every platform with a signed
// engine provisions that binary through binmgr, Windows included. Ranks are
// re-assigned contiguously (self = rank 0) so PlanFor's layer split matches the
// peers that actually join.
func (m *Manager) readyRing(ctx context.Context, base *Ring) *Ring {
	self := Member{Rank: 0, Host: m.self.Host, PeerID: m.self.PeerID}
	if base == nil {
		return &Ring{Members: []Member{self}}
	}
	members := []Member{self}
	for _, member := range base.Members {
		if member.Host == m.self.Host {
			continue
		}
		p := ShardPeer{Host: member.Host, PeerID: member.PeerID}
		rep, err := m.ping(ctx, p)
		if err != nil {
			m.log.Info("shard: peer unreachable, dropping from ring", "host", member.Host, "err", err)
			continue
		}
		if !rep.WorkerBin {
			m.log.Info("shard: peer has no worker binary, dropping from ring", "host", member.Host)
			continue
		}
		members = append(members, Member{Rank: len(members), Host: member.Host, PeerID: member.PeerID})
	}
	return &Ring{Members: members}
}

// provisionPeers asks every candidate peer to land its signed prima engine
// (/provision, best-effort per peer), then polls readiness until every peer
// reports its worker binary or the bounded wait elapses. Peers that stay
// unready — unreachable, or a signature failure that kept the gate closed —
// are left for readyRing to drop. A peer without the endpoint (older build)
// just fails the ask and is polled like the rest.
func (m *Manager) provisionPeers(ctx context.Context, base *Ring) {
	if base == nil {
		return
	}
	var peers []ShardPeer
	for _, member := range base.Members {
		if member.Host == m.self.Host {
			continue
		}
		p := ShardPeer{Host: member.Host, PeerID: member.PeerID}
		peers = append(peers, p)
		askCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := m.askProvision(askCtx, p); err != nil {
			m.log.Info("shard: peer provision ask failed, polling anyway", "host", p.Host, "err", err)
		}
		cancel()
	}
	if len(peers) == 0 {
		return
	}
	wait, poll := m.provisionWait, m.provisionPoll
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	pending := append([]ShardPeer{}, peers...)
	for len(pending) > 0 {
		var still []ShardPeer
		for _, p := range pending {
			rep, err := m.ping(ctx, p)
			if err == nil && rep != nil && rep.WorkerBin {
				continue // provisioned since the ask
			}
			still = append(still, p)
		}
		pending = still
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			m.log.Warn("shard: peers still unprovisioned after bounded wait, gating them out", "hosts", hostsOf(pending))
			return
		}
		if !sleepCtx(ctx, poll) {
			return
		}
	}
}

// isSelfPeer reports whether peer addresses this node itself — by mesh peer
// id, or by host label. Either match short-circuits to the local handler, so
// a trigger naming our own host never mesh-dials ourselves (EOF).
func isSelfPeer(self, peer ShardPeer) bool {
	if peer.PeerID != "" && self.PeerID != "" && peer.PeerID == self.PeerID {
		return true
	}
	return peer.Host != "" && peer.Host == self.Host
}

func hostsOf(peers []ShardPeer) []string {
	hosts := make([]string, 0, len(peers))
	for _, p := range peers {
		hosts = append(hosts, p.Host)
	}
	return hosts
}

// askProvision POSTs to a peer's shard-control /provision over the mesh: land
// your signed prima engine (backgrounded there, 202 here).
func (m *Manager) askProvision(ctx context.Context, peer ShardPeer) error {
	ln, err := m.fwd.Listen(loopback+":0", peer.PeerID, ControlService)
	if err != nil {
		return err
	}
	defer ln.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+"/provision", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("peer %s: provision %s", peer.Host, resp.Status)
	}
	return nil
}

// sleepCtx sleeps for d or until ctx is cancelled; returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// tellWorker forwards to a worker's shard-control endpoint over the mesh and
// POSTs the form request.
func (m *Manager) tellWorker(ctx context.Context, member Member, req FormRequest) error {
	ln, err := m.fwd.Listen(loopback+":0", member.PeerID, ControlService)
	if err != nil {
		return err
	}
	defer ln.Close()

	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+"/form", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("worker control returned %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

// TellLead tells a peer (over the mesh) to LEAD a shard for the model: that node
// becomes the leader, self-provisions, and orchestrates its workers. This is the
// fleet trigger — an agent (or, later, cloudbox on a pool request) starts a shard
// on any node with no ssh, and the system self-drives from there.
func (m *Manager) TellLead(ctx context.Context, peer ShardPeer, model string, apiPort int) error {
	if isSelfPeer(m.self, peer) {
		// Self-trigger: never mesh-dial ourselves — the forwarder can't carry
		// a loopback to our own control endpoint (EOF). Lead from here on a
		// detached context, like the /lead handler does.
		m.log.Info("shard: self-trigger, leading locally", "model", model)
		go func() {
			if err := m.orchestrate(context.Background(), model, apiPort, nil); err != nil {
				m.log.Warn("shard: self-lead orchestrate failed", "model", model, "err", err)
			}
		}()
		return nil
	}
	ln, err := m.fwd.Listen(loopback+":0", peer.PeerID, ControlService)
	if err != nil {
		return err
	}
	defer ln.Close()

	body, _ := json.Marshal(map[string]any{"model": model, "api_port": apiPort})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+"/lead", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("lead %s returned %s", peer.Host, resp.Status)
	}
	return nil
}

// LocalModel is a model present on this node, with its on-disk size.
type LocalModel struct {
	Name  string
	Bytes uint64
}

// MaybeShard is the auto-trigger: for the first local model too big to serve on
// this node alone (but a same-LAN ring exists to spread it), it orchestrates a
// shard with this node as leader. Idempotent — skips the already-active model.
// The daemon calls this with the local ollama models + this node's memory budget.
func (m *Manager) MaybeShard(ctx context.Context, localModels []LocalModel, localBytes uint64, apiPort int) error {
	if localBytes == 0 || m.Ring() == nil {
		return nil // no budget configured, or no same-LAN peers to shard across
	}
	active := m.ActiveModel()
	for _, lm := range localModels {
		if lm.Name == active {
			return nil // already serving this sharded model
		}
		if lm.Bytes > localBytes {
			m.log.Info("shard: auto-trigger", "model", lm.Name, "bytes", lm.Bytes, "local_budget", localBytes)
			return m.autoShard(ctx, lm, localBytes, apiPort)
		}
	}
	return nil
}
