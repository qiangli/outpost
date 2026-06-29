package shard

import "context"

// Decider chooses whether to shard a model and which node leads, from the
// gathered fleet state. The default is DecideShard — deterministic, most-VRAM
// leads. It is the *bootstrap*: always available, no dependency, so a shard can
// always form even when the pool is busy or the model is unsure.
//
// The "self-think" path the mesh can grow plugs in here as an alternate Decider:
// package this same state (the per-node capacities + the model) into a prompt and
// ask the pooled LLM (cloudbox ollama) to refine the partition / leader choice.
// Bootstrapped by the deterministic Decider — the mesh has to be up to run the
// LLM that decides how to organize the mesh, so it self-organizes deterministically
// first, then self-optimizes. The orchestrator gathers the state via PingPeer.
type Decider func(modelBytes uint64, nodes []NodeCapacity) Decision

// gatherViaPing collects candidate capacities by pinging each ring peer over the
// mesh — the ping doubles as the readiness gate, so an unreachable peer drops out
// of contention. It is the default capacity source for the election.
func (m *Manager) gatherViaPing(ctx context.Context, modelBytes, selfBudget uint64) ([]NodeCapacity, map[string]ShardPeer) {
	ring := m.Ring()
	if ring == nil {
		return nil, nil
	}
	nodes := []NodeCapacity{{Host: m.self.Host, Bytes: selfBudget}}
	peers := make(map[string]ShardPeer)
	for _, member := range ring.Members {
		if member.Host == m.self.Host {
			continue
		}
		p := ShardPeer{Host: member.Host, PeerID: member.PeerID}
		rep, err := m.PingPeer(ctx, p)
		if err != nil {
			continue // unreachable → out of contention
		}
		nodes = append(nodes, NodeCapacity{Host: member.Host, Bytes: rep.BudgetBytes})
		peers[member.Host] = p
	}
	return nodes, peers
}

// elect gathers candidate capacities and runs the Decider — no human chooses the
// leader.
func (m *Manager) elect(ctx context.Context, modelBytes, selfBudget uint64) (Decision, map[string]ShardPeer) {
	nodes, peers := m.gather(ctx, modelBytes, selfBudget)
	if len(nodes) == 0 {
		return Decision{}, nil
	}
	return m.decide(modelBytes, nodes), peers
}

// autoShard is the autonomous trigger body: elect a leader by capacity (no human
// chooses), then lead if elected. When a more-capable peer is elected, this node
// defers — that peer self-elects when its own trigger sees the model. (Cross-node
// hand-off + model distribution to the elected leader are a follow-on.)
func (m *Manager) autoShard(ctx context.Context, lm LocalModel, selfBudget uint64, apiPort int) error {
	decision, peers := m.elect(ctx, lm.Bytes, selfBudget)
	if !decision.ShouldShard {
		return nil
	}
	if decision.Leader == m.self.Host {
		m.log.Info("shard: elected self as leader", "model", lm.Name, "candidates", len(peers)+1, "reason", decision.Reason)
		return m.orchestrate(ctx, lm.Name, apiPort, nil)
	}
	m.log.Info("shard: deferring to elected peer leader", "model", lm.Name, "leader", decision.Leader)
	return nil
}
