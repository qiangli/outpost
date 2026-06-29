package shard

// NodeCapacity is one candidate node's usable model-memory budget (VRAM plus any
// spillover RAM the engine can use) for shard placement.
type NodeCapacity struct {
	Host  string
	Bytes uint64
}

// Decision is the auto-trigger's verdict for a model of a given size against a
// set of candidate nodes.
type Decision struct {
	ShouldShard bool   // true → too big for one node but fits the pooled total
	Leader      string // host that should lead (most capacity); "" when not sharding
	Reason      string
}

// DecideShard decides whether a model of modelBytes should be served sharded
// across the candidate nodes, and which node leads:
//
//   - fits on the single biggest node       → no shard (the pool routes to it)
//   - bigger than one node, ≤ pooled total   → shard; leader = most-capacity node
//   - bigger than the pooled total           → can't serve (no shard)
//
// Leader = the most-capacity (most-VRAM) node — it holds the largest contiguous
// layer span and drives generation — matching the zero-config "most-VRAM host
// leads" default.
func DecideShard(modelBytes uint64, nodes []NodeCapacity) Decision {
	if modelBytes == 0 {
		return Decision{Reason: "unknown model size"}
	}
	if len(nodes) == 0 {
		return Decision{Reason: "no candidate nodes"}
	}
	var total, maxCap uint64
	var leader string
	for _, n := range nodes {
		total += n.Bytes
		if n.Bytes > maxCap {
			maxCap = n.Bytes
			leader = n.Host
		}
	}
	switch {
	case modelBytes <= maxCap:
		return Decision{Reason: "fits on a single node; the pool serves it directly"}
	case modelBytes > total:
		return Decision{Reason: "model exceeds the pooled capacity; cannot serve"}
	default:
		return Decision{ShouldShard: true, Leader: leader, Reason: "too big for one node; sharding across the pool"}
	}
}
