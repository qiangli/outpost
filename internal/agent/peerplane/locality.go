package peerplane

// BestTier reduces a measured peer snapshot to this host's effective
// locality tier: the lowest-latency class observed to ANY reachable
// peer. The rationale is "best-link wins" — a host with at least one
// sub-2ms (tp) peer link is itself tensor-parallel eligible; one whose
// peers are all LAN is lan; and so on. An empty snapshot (single
// machine, or no probe cycle has completed yet) returns TierUnreached,
// and the caller decides whether to render or omit a "no peers" tier.
func BestTier(snap []PeerTier) Tier {
	best := TierUnreached
	for _, pt := range snap {
		if tierRank(pt.Tier) > tierRank(best) {
			best = pt.Tier
		}
	}
	return best
}

// tierRank orders tiers by locality (higher = more local / lower-latency)
// so BestTier can pick the strongest measured link. Unknown values sort
// with TierUnreached at the bottom.
func tierRank(t Tier) int {
	switch t {
	case TierTP:
		return 3
	case TierLAN:
		return 2
	case TierWAN:
		return 1
	default: // TierUnreached / unknown
		return 0
	}
}

// SelfTier returns the host's effective locality tier from the latest
// measured snapshot — a convenience wrapper over BestTier(s.Snapshot()).
// Use this to stamp the host's own Node with its measured (not guessed)
// locality. Returns TierUnreached until the first probe cycle records a
// reachable peer.
func (s *Service) SelfTier() Tier {
	return BestTier(s.Snapshot())
}
