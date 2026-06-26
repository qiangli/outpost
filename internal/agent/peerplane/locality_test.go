package peerplane

import "testing"

func TestBestTier(t *testing.T) {
	cases := []struct {
		name string
		snap []PeerTier
		want Tier
	}{
		{"empty", nil, TierUnreached},
		{"single tp", []PeerTier{{Host: "a", Tier: TierTP}}, TierTP},
		{"single lan", []PeerTier{{Host: "a", Tier: TierLAN}}, TierLAN},
		{"single wan", []PeerTier{{Host: "a", Tier: TierWAN}}, TierWAN},
		{"single unreached", []PeerTier{{Host: "a", Tier: TierUnreached}}, TierUnreached},
		{
			"best wins: tp over lan/wan",
			[]PeerTier{{Host: "a", Tier: TierWAN}, {Host: "b", Tier: TierTP}, {Host: "c", Tier: TierLAN}},
			TierTP,
		},
		{
			"best wins: lan over wan/unreached",
			[]PeerTier{{Host: "a", Tier: TierUnreached}, {Host: "b", Tier: TierWAN}, {Host: "c", Tier: TierLAN}},
			TierLAN,
		},
		{
			"all unreached",
			[]PeerTier{{Host: "a", Tier: TierUnreached}, {Host: "b", Tier: TierUnreached}},
			TierUnreached,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BestTier(tc.snap); got != tc.want {
				t.Errorf("BestTier(%+v) = %q, want %q", tc.snap, got, tc.want)
			}
		})
	}
}

func TestServiceSelfTier(t *testing.T) {
	s := New(Config{})
	if got := s.SelfTier(); got != TierUnreached {
		t.Errorf("SelfTier with no measurements = %q, want %q", got, TierUnreached)
	}

	s.mu.Lock()
	s.tiers["peer-a"] = PeerTier{Host: "peer-a", Tier: TierUnreached}
	s.tiers["peer-b"] = PeerTier{Host: "peer-b", Tier: TierLAN}
	s.tiers["peer-c"] = PeerTier{Host: "peer-c", Tier: TierTP}
	s.mu.Unlock()

	if got := s.SelfTier(); got != TierTP {
		t.Errorf("SelfTier = %q, want %q (best of measured peers)", got, TierTP)
	}
}
