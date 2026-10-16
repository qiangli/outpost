package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/fleet"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

const (
	testSeedOllama = `name: ollama
kind: service
description: per-host Ollama proxy
provider: internal
capability: outpost.ollama
owner: outpost
enabled: false
`
	testSeedOllamaPool = `name: ollama-pool
kind: service
description: cloudbox LLM pool participation
provider: internal
capability: outpost.ollama-pool
owner: outpost
requires: [ollama]
enabled: false
`
	testSeedShard = `name: shard
kind: service
description: intra-home model sharding
provider: internal
capability: outpost.shard
owner: outpost
enabled: false
`
)

func seedOn(name string) string {
	return "name: " + name + "\nkind: service\nprovider: internal\ncapability: outpost." + name + "\nowner: outpost\nenabled: true\n"
}

func newTestMigrationStore(t *testing.T) *svcunit.Store {
	t.Helper()
	root := t.TempDir()
	baseFS := fstest.MapFS{
		"baseline/services/ollama.yaml":      &fstest.MapFile{Data: []byte(testSeedOllama)},
		"baseline/services/ollama-pool.yaml": &fstest.MapFile{Data: []byte(testSeedOllamaPool)},
		"baseline/services/shard.yaml":       &fstest.MapFile{Data: []byte(testSeedShard)},
		"baseline/services/podman.yaml":      &fstest.MapFile{Data: []byte(seedOn("podman"))},
		"baseline/services/files.yaml":       &fstest.MapFile{Data: []byte(seedOn("files"))},
		"baseline/services/otel.yaml":        &fstest.MapFile{Data: []byte(seedOn("otel"))},
		"baseline/services/warm.yaml":        &fstest.MapFile{Data: []byte(seedOn("warm"))},
	}
	cat := fleet.New(fleet.WithRoot(root), fleet.WithBaselineFS(baseFS), fleet.WithoutCloudOverlay())
	return &svcunit.Store{Catalog: cat}
}

// TestLegacyServiceMigration_OldConfig asserts that upgrading a paired host with
// legacy agent.json keys (ollama_enabled, ollama_pool_enabled, shard.enabled)
// creates local service unit overrides that preserve explicit false and true,
// and drops the retired keys from agent.json.
func TestLegacyServiceMigration_OldConfig(t *testing.T) {
	st := newTestMigrationStore(t)
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.json")

	// Upgraded paired host with explicit false for ollama, true for pool and shard.
	oldJSON := []byte(`{
		"agent_name": "paired-box",
		"access_token": "valid-token-123",
		"server_addr": "ai.dhnt.io",
		"server_port": 443,
		"future_operator_setting": {"keep": true},
		"ollama_enabled": false,
		"ollama_pool_enabled": true,
		"shard": {
			"enabled": true,
			"role": "leader"
		}
	}`)
	if err := os.WriteFile(cfgPath, oldJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	migrated, err := migrateLegacyServices(cfgPath, fc, st)
	if err != nil {
		t.Fatalf("migrateLegacyServices: %v", err)
	}
	if !migrated {
		t.Fatalf("expected migrated=true for old config with legacy keys")
	}

	// 1. Verify ollama unit has override with explicit false.
	ollama, ok := st.Unit("ollama")
	if !ok {
		t.Fatal("unit ollama not found")
	}
	if ollama.Enabled != false {
		t.Errorf("ollama.Enabled = %v, want false (explicit false preserved)", ollama.Enabled)
	}
	if ollama.Ring != assetring.RingLocal {
		t.Errorf("ollama.Ring = %v, want %v (local override)", ollama.Ring, assetring.RingLocal)
	}

	// 2. Verify ollama-pool unit has override with explicit true.
	pool, ok := st.Unit("ollama-pool")
	if !ok {
		t.Fatal("unit ollama-pool not found")
	}
	if pool.Enabled != true {
		t.Errorf("ollama-pool.Enabled = %v, want true (explicit true preserved)", pool.Enabled)
	}
	if pool.Ring != assetring.RingLocal {
		t.Errorf("ollama-pool.Ring = %v, want %v (local override)", pool.Ring, assetring.RingLocal)
	}

	// 3. Verify shard unit has override with explicit true.
	shard, ok := st.Unit("shard")
	if !ok {
		t.Fatal("unit shard not found")
	}
	if shard.Enabled != true {
		t.Errorf("shard.Enabled = %v, want true (explicit true preserved)", shard.Enabled)
	}
	if shard.Ring != assetring.RingLocal {
		t.Errorf("shard.Ring = %v, want %v (local override)", shard.Ring, assetring.RingLocal)
	}

	// 4. Verify agent.json on disk dropped the legacy keys.
	savedBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(savedBytes, &raw); err != nil {
		t.Fatalf("unmarshal saved config: %v", err)
	}
	for _, key := range []string{"ollama_enabled", "ollama_pool_enabled"} {
		if _, exists := raw[key]; exists {
			t.Errorf("legacy key %q survived in agent.json after migration", key)
		}
	}
	shardRaw, ok := raw["shard"].(map[string]any)
	if !ok || shardRaw["role"] != "leader" {
		t.Errorf("unrelated shard role lost: %+v", raw["shard"])
	} else if _, exists := shardRaw["enabled"]; exists {
		t.Errorf("retired shard.enabled survived: %+v", shardRaw)
	}
	if future, ok := raw["future_operator_setting"].(map[string]any); !ok || future["keep"] != true {
		t.Errorf("unknown operator field lost: %+v", raw["future_operator_setting"])
	}
	if raw["agent_name"] != "paired-box" || raw["access_token"] != "valid-token-123" {
		t.Errorf("surviving fields corrupted: %+v", raw)
	}
}

// TestLegacyServiceMigration_PreservesExistingOperatorOverrides asserts that
// an existing operator service override (in the local ring) takes precedence over
// legacy agent.json values and is not clobbered.
func TestLegacyServiceMigration_PreservesExistingOperatorOverrides(t *testing.T) {
	st := newTestMigrationStore(t)
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.json")

	// Operator previously explicitly enabled ollama and disabled shard locally.
	ollamaBefore, _ := st.Unit("ollama")
	ollamaBefore.Enabled = true
	if err := st.Save(ollamaBefore); err != nil {
		t.Fatal(err)
	}
	shardBefore, _ := st.Unit("shard")
	shardBefore.Enabled = false
	if err := st.Save(shardBefore); err != nil {
		t.Fatal(err)
	}

	// Old agent.json says the opposite: ollama_enabled=false, shard.enabled=true.
	oldJSON := []byte(`{
		"agent_name": "paired-box",
		"access_token": "valid-token-123",
		"ollama_enabled": false,
		"shard": {
			"enabled": true
		}
	}`)
	if err := os.WriteFile(cfgPath, oldJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacyServices(cfgPath, fc, st)
	if err != nil {
		t.Fatalf("migrateLegacyServices: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true")
	}

	// Operator's existing local override for ollama (true) was preserved!
	ollamaAfter, _ := st.Unit("ollama")
	if ollamaAfter.Enabled != true {
		t.Errorf("ollama.Enabled = %v, want true (operator override preserved)", ollamaAfter.Enabled)
	}

	// Operator's existing local override for shard (false) was preserved!
	shardAfter, _ := st.Unit("shard")
	if shardAfter.Enabled != false {
		t.Errorf("shard.Enabled = %v, want false (operator override preserved)", shardAfter.Enabled)
	}
}

// TestLegacyServiceMigration_FreshPairedHost asserts that a fresh paired host
// (no legacy keys in agent.json) does not have any overrides written.
func TestLegacyServiceMigration_FreshPairedHost(t *testing.T) {
	st := newTestMigrationStore(t)
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.json")

	// Fresh paired host config — zero legacy keys.
	freshJSON := []byte(`{
		"agent_name": "fresh-box",
		"access_token": "token-abc",
		"server_addr": "ai.dhnt.io",
		"server_port": 443
	}`)
	if err := os.WriteFile(cfgPath, freshJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacyServices(cfgPath, fc, st)
	if err != nil {
		t.Fatalf("migrateLegacyServices: %v", err)
	}
	if migrated {
		t.Errorf("migrated = true, want false for fresh paired host")
	}

	// No local overrides should be created in the unit store.
	for _, name := range []string{"ollama", "ollama-pool", "shard"} {
		u, ok := st.Unit(name)
		if !ok {
			t.Fatalf("unit %s not found", name)
		}
		if u.Ring == assetring.RingLocal {
			t.Errorf("unit %s has local override on fresh paired host", name)
		}
	}
}

// TestLegacyServiceMigration_RepeatMigration asserts that repeating migration
// is completely idempotent and does not alter units or config.
func TestLegacyServiceMigration_RepeatMigration(t *testing.T) {
	st := newTestMigrationStore(t)
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.json")

	oldJSON := []byte(`{
		"agent_name": "paired-box",
		"access_token": "valid-token-123",
		"ollama_enabled": true,
		"shard": {
			"enabled": false
		}
	}`)
	if err := os.WriteFile(cfgPath, oldJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// First migration run.
	migrated1, err := migrateLegacyServices(cfgPath, fc, st)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !migrated1 {
		t.Fatal("first run: expected migrated=true")
	}

	ollama1, _ := st.Unit("ollama")
	shard1, _ := st.Unit("shard")

	// Second migration run (repeat migration).
	fc2, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	migrated2, err := migrateLegacyServices(cfgPath, fc2, st)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if migrated2 {
		t.Errorf("second run: migrated=true, want false (repeat migration is a no-op)")
	}

	ollama2, _ := st.Unit("ollama")
	shard2, _ := st.Unit("shard")

	if ollama1.Enabled != ollama2.Enabled || ollama2.Enabled != true {
		t.Errorf("ollama state changed: before=%v, after=%v", ollama1.Enabled, ollama2.Enabled)
	}
	if shard1.Enabled != shard2.Enabled || shard2.Enabled != false {
		t.Errorf("shard state changed: before=%v, after=%v", shard1.Enabled, shard2.Enabled)
	}
}

// TestLegacyServiceMigration_UnpairedHostDoesNotExposeServices asserts that an
// unpaired host (missing access_token or agent_name) is never migrated and
// does not have services enabled or exposed.
func TestLegacyServiceMigration_UnpairedHostDoesNotExposeServices(t *testing.T) {
	st := newTestMigrationStore(t)
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.json")

	// Unpaired host has no access_token.
	unpairedJSON := []byte(`{
		"agent_name": "unpaired-box",
		"access_token": "",
		"ollama_enabled": true,
		"ollama_pool_enabled": true,
		"shard": {
			"enabled": true
		}
	}`)
	if err := os.WriteFile(cfgPath, unpairedJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacyServices(cfgPath, fc, st)
	if err != nil {
		t.Fatalf("migrateLegacyServices: %v", err)
	}
	if migrated {
		t.Errorf("migrated=true on unpaired host, want false")
	}

	// No units should have been enabled or overridden in local ring.
	for _, name := range []string{"ollama", "ollama-pool", "shard"} {
		u, ok := st.Unit(name)
		if !ok {
			t.Fatalf("unit %s not found", name)
		}
		if u.Ring == assetring.RingLocal {
			t.Errorf("unit %s has local override on unpaired host", name)
		}
		if u.Enabled {
			t.Errorf("unit %s enabled on unpaired host", name)
		}
	}
}

// TestLegacyServiceMigration_DirectShardBooleanAndPartial asserts that shard can be specified
// as a boolean directly, and that partial presence of legacy keys migrates only those present.
func TestLegacyServiceMigration_DirectShardBooleanAndPartial(t *testing.T) {
	st := newTestMigrationStore(t)
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.json")

	// Only shard is present as a direct boolean false, and ollama_enabled as true.
	// ollama_pool_enabled is absent.
	partialJSON := []byte(`{
		"agent_name": "paired-box",
		"access_token": "valid-token-123",
		"ollama_enabled": true,
		"shard": false
	}`)
	if err := os.WriteFile(cfgPath, partialJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacyServices(cfgPath, fc, st)
	if err != nil {
		t.Fatalf("migrateLegacyServices: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true")
	}

	ollama, _ := st.Unit("ollama")
	if ollama.Ring != assetring.RingLocal || ollama.Enabled != true {
		t.Errorf("ollama: ring=%v, enabled=%v; want local, true", ollama.Ring, ollama.Enabled)
	}

	shard, _ := st.Unit("shard")
	if shard.Ring != assetring.RingLocal || shard.Enabled != false {
		t.Errorf("shard: ring=%v, enabled=%v; want local, false", shard.Ring, shard.Enabled)
	}

	// ollama-pool was not present in legacy config, so it should not have an override.
	pool, _ := st.Unit("ollama-pool")
	if pool.Ring == assetring.RingLocal {
		t.Errorf("ollama-pool should not have local override when not present in legacy config")
	}
}

// TestLegacyServiceMigration_NullValuesIgnored asserts that null values for legacy keys
// are treated as absent, not triggering overrides.
func TestLegacyServiceMigration_NullValuesIgnored(t *testing.T) {
	st := newTestMigrationStore(t)
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.json")

	nullJSON := []byte(`{
		"agent_name": "paired-box",
		"access_token": "valid-token-123",
		"ollama_enabled": null,
		"shard": null
	}`)
	if err := os.WriteFile(cfgPath, nullJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacyServices(cfgPath, fc, st)
	if err != nil {
		t.Fatalf("migrateLegacyServices: %v", err)
	}
	if migrated {
		t.Errorf("migrated=true for null values, want false")
	}

	for _, name := range []string{"ollama", "ollama-pool", "shard"} {
		u, _ := st.Unit(name)
		if u.Ring == assetring.RingLocal {
			t.Errorf("unit %s should not have local override for null value", name)
		}
	}
}

func TestLegacyServiceMigration_BaselineUnits(t *testing.T) {
	cases := []struct {
		name         string
		json         string
		unit         string
		wantEnabled  bool
		wantLocal    bool
		wantSettings map[string]string
	}{
		{"podman false", `"podman_enabled": false`, "podman", false, true, nil},
		{"podman true", `"podman_enabled": true`, "podman", true, true, nil},
		{"podman absent", `"unrelated": 1`, "podman", true, false, nil},
		{"files false", `"files_enabled": false`, "files", false, true, nil},
		{"files true", `"files_enabled": true`, "files", true, true, nil},
		{"otel false", `"otel_enabled": false`, "otel", false, true, nil},
		{"otel pool only", `"otel_pool_enabled": false`, "otel", true, true, map[string]string{"pool": "false"}},
		{"otel both", `"otel_enabled": true, "otel_pool_enabled": true`, "otel", true, true, map[string]string{"pool": "true"}},
		{"warm false", `"warm_serving_enabled": false`, "warm", false, true, nil},
		{"warm frac only", `"warm_budget_frac": 0.25`, "warm", true, true, map[string]string{"budget_frac": "0.25"}},
		{"warm both", `"warm_serving_enabled": false, "warm_budget_frac": 0.5`, "warm", false, true, map[string]string{"budget_frac": "0.5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestMigrationStore(t)
			cfgPath := filepath.Join(t.TempDir(), "agent.json")
			body := `{"agent_name":"paired-box","access_token":"tok","files_signing_key":"c2VjcmV0","warm_desired":["m1"],` + tc.json + `}`
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			fc, err := conf.LoadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			migrated, err := migrateLegacyServices(cfgPath, fc, st)
			if err != nil {
				t.Fatal(err)
			}
			u, ok := st.Unit(tc.unit)
			if !ok {
				t.Fatalf("unit %s missing", tc.unit)
			}
			if u.Enabled != tc.wantEnabled {
				t.Errorf("Enabled = %v, want %v", u.Enabled, tc.wantEnabled)
			}
			if got := u.Ring == assetring.RingLocal; got != tc.wantLocal {
				t.Errorf("local override = %v, want %v (migrated=%v)", got, tc.wantLocal, migrated)
			}
			for k, v := range tc.wantSettings {
				if u.Settings[k] != v {
					t.Errorf("settings[%s] = %q, want %q", k, u.Settings[k], v)
				}
			}
			data, _ := os.ReadFile(cfgPath)
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			for _, k := range legacyRetiredKeys {
				if _, ok := raw[k]; ok {
					t.Errorf("retired key %s survived", k)
				}
			}
			if raw["files_signing_key"] != "c2VjcmV0" || raw["warm_desired"] == nil {
				t.Errorf("daemon state keys dropped: %+v", raw)
			}
			// Repeat run is a no-op.
			again, err := migrateLegacyServices(cfgPath, nil, st)
			if err != nil || again {
				t.Errorf("repeat migrated=%v err=%v, want no-op", again, err)
			}
		})
	}
}

func TestLegacyServiceMigration_BaselineLocalOverrideWins(t *testing.T) {
	st := newTestMigrationStore(t)
	u, _ := st.Unit("podman")
	u.Enabled = false
	if err := st.Save(u); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	body := `{"agent_name":"b","access_token":"t","podman_enabled":true}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateLegacyServices(cfgPath, nil, st); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Unit("podman"); got.Enabled {
		t.Error("legacy podman_enabled=true clobbered the local override")
	}
}

func TestLegacyServiceMigration_BaselineUnpairedUntouched(t *testing.T) {
	st := newTestMigrationStore(t)
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	body := `{"otel_enabled":false}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := migrateLegacyServices(cfgPath, nil, st)
	if err != nil || migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
	if got, _ := st.Unit("otel"); !got.Enabled {
		t.Error("unpaired host's otel unit was changed")
	}
}
