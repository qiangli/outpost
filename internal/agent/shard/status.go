package shard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

// StatusReport is a node's shard-readiness, returned over the mesh shard-control
// /status endpoint. It is the app-level ping/pong: a successful fetch IS the
// reachability proof (the peer's daemon answered over the mesh), and the body
// carries the facts the orchestrator — or an operator — needs to decide whether
// the peer can take a rank, with no ssh into the box.
type StatusReport struct {
	Host        string       `json:"host"`
	Models      []LocalModel `json:"models,omitempty"`
	BudgetBytes uint64       `json:"budget_bytes"`
	ServerBin   bool         `json:"server_bin"` // prima llama-server present on disk
	WorkerBin   bool         `json:"worker_bin"` // prima llama-cli present on disk
	ActiveModel string       `json:"active_model,omitempty"`
	RingMembers int          `json:"ring_members"`
}

// LocalStatus builds this node's report from live manager state (no network).
func (m *Manager) LocalStatus() StatusReport {
	var models []LocalModel
	var budget uint64
	if m.localLoad != nil {
		models, budget = m.localLoad()
	}
	rep := StatusReport{
		Host:        m.self.Host,
		Models:      models,
		BudgetBytes: budget,
		ServerBin:   fileExists(m.bins.ServerBin),
		WorkerBin:   fileExists(m.bins.WorkerBin),
		ActiveModel: m.ActiveModel(),
	}
	if r := m.Ring(); r != nil {
		rep.RingMembers = len(r.Members)
	}
	return rep
}

// PingPeer forwards to a peer's shard-control /status over the mesh and returns
// its report — app-level reachability + readiness, no ssh. The caller resolves
// host→PeerID (cloudbox peer/connect), so it works for any paired peer, not just
// same-LAN ones.
func (m *Manager) PingPeer(ctx context.Context, peer ShardPeer) (*StatusReport, error) {
	ln, err := m.fwd.Listen(loopback+":0", peer.PeerID, ControlService)
	if err != nil {
		return nil, err
	}
	defer ln.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s: status %s", peer.Host, resp.Status)
	}
	var rep StatusReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
