package conf

import "testing"

func bp(b bool) *bool { return &b }

// Zero-config contract: an owner-registered (paired) Ollama-on node shards by
// default; the only knob is opt-out.
func TestShardOn_ZeroConfig(t *testing.T) {
	cases := []struct {
		name string
		fc   *FileConfig
		want bool
	}{
		{"paired ollama node, no shard cfg → zero-config ON",
			&FileConfig{OllamaEnabled: true, AccessToken: "tok"}, true},
		{"explicit opt-out (Enabled=false)",
			&FileConfig{OllamaEnabled: true, AccessToken: "tok", Shard: &ShardConfig{Enabled: bp(false)}}, false},
		{"explicit force-on (Enabled=true)",
			&FileConfig{OllamaEnabled: true, AccessToken: "tok", Shard: &ShardConfig{Enabled: bp(true)}}, true},
		{"not paired (no access token) → off",
			&FileConfig{OllamaEnabled: true}, false},
		{"ollama off → off",
			&FileConfig{AccessToken: "tok"}, false},
		{"nil config → off", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.fc.ShardOn(); got != c.want {
				t.Errorf("ShardOn() = %v, want %v", got, c.want)
			}
		})
	}
}

// The mesh is the shard's transport, so enabling sharding must imply the mesh
// is needed — zero-config sharding brings its own data plane up.
func TestMeshNeeded_ShardCascade(t *testing.T) {
	shardOn := &FileConfig{OllamaEnabled: true, AccessToken: "tok"} // zero-config shard on, mesh nil
	if !shardOn.ShardOn() {
		t.Fatal("precondition: ShardOn should be true")
	}
	if !shardOn.MeshNeeded() {
		t.Error("MeshNeeded must be true when sharding is on (cascade)")
	}
	if !shardOn.PeerPlaneNeeded() {
		t.Error("PeerPlaneNeeded must be true when sharding is on (cascade)")
	}

	meshOnly := &FileConfig{MeshEnabled: bp(true), OllamaEnabled: true, AccessToken: "tok", Shard: &ShardConfig{Enabled: bp(false)}}
	if !meshOnly.MeshNeeded() {
		t.Error("MeshNeeded must be true when mesh is explicitly on")
	}

	// Mesh now defaults ON for paired hosts (zero-config peer reachability), so an
	// unset mesh flag still needs the mesh even with sharding off.
	defaultOn := &FileConfig{OllamaEnabled: true, AccessToken: "tok", Shard: &ShardConfig{Enabled: bp(false)}}
	if !defaultOn.MeshNeeded() {
		t.Error("MeshNeeded must be true by default (mesh defaults ON when the flag is unset)")
	}

	// Explicit opt-out (mesh_enabled=false) with sharding off is the only way off.
	optedOut := &FileConfig{MeshEnabled: bp(false), OllamaEnabled: true, AccessToken: "tok", Shard: &ShardConfig{Enabled: bp(false)}}
	if optedOut.MeshNeeded() {
		t.Error("MeshNeeded must be false when mesh is explicitly opted out and shard is off")
	}
}
