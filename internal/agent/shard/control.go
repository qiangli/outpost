package shard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
		if err := m.onForm(r.Context(), &req.Ring, req.MyRank, m.serveConfig(req)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
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
	ring := m.Ring()
	if ring == nil {
		return fmt.Errorf("shard: no candidate ring (no same-LAN peers)")
	}
	for _, member := range ring.Members {
		if member.Rank == 0 {
			continue // self = leader; formed last, below
		}
		req := FormRequest{Ring: *ring, MyRank: member.Rank, Model: model, APIPort: apiPort, Extra: extra}
		if err := m.tellWorker(ctx, member, req); err != nil {
			return fmt.Errorf("shard: form worker %s (rank %d): %w", member.Host, member.Rank, err)
		}
	}
	return m.onForm(ctx, ring, 0, ServeConfig{
		Model: model, ServerBin: m.bins.ServerBin, WorkerBin: m.bins.WorkerBin, APIPort: apiPort, Extra: extra,
	})
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
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("worker control returned %s", resp.Status)
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
			return m.orchestrate(ctx, lm.Name, apiPort, nil)
		}
	}
	return nil
}
