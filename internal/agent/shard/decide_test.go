package shard

import "testing"

func TestDecideShard(t *testing.T) {
	const GB = 1 << 30
	nodes := []NodeCapacity{
		{Host: "big", Bytes: 24 * GB},
		{Host: "mid", Bytes: 16 * GB},
		{Host: "small", Bytes: 8 * GB},
	} // biggest 24, pooled total 48

	cases := []struct {
		name      string
		bytes     uint64
		nodes     []NodeCapacity
		wantShard bool
		wantLead  string
	}{
		{"fits on biggest → no shard", 20 * GB, nodes, false, ""},
		{"exactly biggest → no shard", 24 * GB, nodes, false, ""},
		{"too big for one, fits pool → shard, leader=big", 40 * GB, nodes, true, "big"},
		{"exceeds pool → no shard", 64 * GB, nodes, false, ""},
		{"unknown size → no shard", 0, nodes, false, ""},
		{"no nodes → no shard", 40 * GB, nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DecideShard(c.bytes, c.nodes)
			if d.ShouldShard != c.wantShard {
				t.Errorf("ShouldShard = %v, want %v (reason: %s)", d.ShouldShard, c.wantShard, d.Reason)
			}
			if d.Leader != c.wantLead {
				t.Errorf("Leader = %q, want %q", d.Leader, c.wantLead)
			}
		})
	}
}
