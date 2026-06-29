package shard

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/outpost/internal/agent/brain"
)

// Decider chooses whether to shard a model and which node leads, from the
// gathered fleet state. The default is DecideShard — deterministic, most-VRAM
// leads. It is the *bootstrap*: always available, no dependency, so a shard can
// always form. The brain (the pooled-LLM Refiner) refines this choice when wired;
// the deterministic Decider is what it bootstraps from.
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

// elect gathers candidate capacities, decides deterministically (the bootstrap),
// then lets the brain refine the leader via the pooled LLM when wired — the
// bootstrap always stands. No human chooses.
func (m *Manager) elect(ctx context.Context, modelBytes, selfBudget uint64) (Decision, map[string]ShardPeer) {
	nodes, peers := m.gather(ctx, modelBytes, selfBudget)
	if len(nodes) == 0 {
		return Decision{}, nil
	}
	bootstrap := m.decide(modelBytes, nodes)
	final, fromLLM := brain.Decide(ctx, bootstrap, shardPrompt(modelBytes, nodes), m.refiner, parseShardLeader(bootstrap, nodes))
	if fromLLM {
		m.log.Info("shard: brain refined the leader", "leader", final.Leader)
	}
	return final, peers
}

// shardPrompt frames the leader decision for the pooled LLM.
func shardPrompt(modelBytes uint64, nodes []NodeCapacity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A model of %d bytes must be served sharded. Candidate nodes (host: model-memory budget in bytes):\n", modelBytes)
	for _, n := range nodes {
		fmt.Fprintf(&b, "- %s: %d\n", n.Host, n.Bytes)
	}
	b.WriteString("Which single host should LEAD (serve the OpenAI endpoint + drive generation)? Reply with only the host name.")
	return b.String()
}

// parseShardLeader turns the LLM's leader pick into a Decision, but only when the
// pick is a real candidate AND the bootstrap already decided to shard — so the
// brain refines the leader, never overrides the deterministic fits-or-not verdict.
func parseShardLeader(bootstrap Decision, nodes []NodeCapacity) func(string) (Decision, bool) {
	return func(reply string) (Decision, bool) {
		if !bootstrap.ShouldShard {
			return Decision{}, false
		}
		choice := strings.TrimSpace(reply)
		for _, n := range nodes {
			if n.Host == choice {
				return Decision{ShouldShard: true, Leader: choice, Reason: "brain-elected leader"}, true
			}
		}
		return Decision{}, false
	}
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
