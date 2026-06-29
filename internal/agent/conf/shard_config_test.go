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

	meshOnly := &FileConfig{MeshEnabled: bp(true), OllamaEnabled: true, AccessToken: "tok", Shard: &ShardConfig{Enabled: bp(false)}}
	if !meshOnly.MeshNeeded() {
		t.Error("MeshNeeded must be true when mesh is explicitly on")
	}

	bothOff := &FileConfig{OllamaEnabled: true, AccessToken: "tok", Shard: &ShardConfig{Enabled: bp(false)}}
	if bothOff.MeshNeeded() {
		t.Error("MeshNeeded must be false when both mesh and shard are off")
	}
}
