package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/qiangli/yoke/pkg/assetring"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

type legacyServiceConfig struct {
	OllamaEnabled     *bool
	OllamaPoolEnabled *bool
	ShardEnabled      *bool
}

// extractLegacyServiceConfig inspects raw JSON for legacy agent.json keys:
// ollama_enabled, ollama_pool_enabled, and shard.enabled.
func extractLegacyServiceConfig(data []byte) (*legacyServiceConfig, bool) {
	if len(data) == 0 {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}

	var cfg legacyServiceConfig
	var found bool

	if v, ok := raw["ollama_enabled"]; ok {
		var b *bool
		if err := json.Unmarshal(v, &b); err == nil && b != nil {
			cfg.OllamaEnabled = b
			found = true
		}
	}
	if v, ok := raw["ollama_pool_enabled"]; ok {
		var b *bool
		if err := json.Unmarshal(v, &b); err == nil && b != nil {
			cfg.OllamaPoolEnabled = b
			found = true
		}
	}
	if v, ok := raw["shard"]; ok {
		var obj struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.Unmarshal(v, &obj); err == nil && obj.Enabled != nil {
			cfg.ShardEnabled = obj.Enabled
			found = true
		} else {
			var b *bool
			if err := json.Unmarshal(v, &b); err == nil && b != nil {
				cfg.ShardEnabled = b
				found = true
			}
		}
	}

	if !found {
		return nil, false
	}
	return &cfg, true
}

// migrateLegacyServices performs a one-time, idempotent compatibility migration
// of legacy agent.json ollama_enabled, ollama_pool_enabled, and shard.enabled
// into service unit overrides on upgraded paired hosts.
//
// Explicit true/false choices from legacy agent.json are preserved as unit overrides,
// while higher-priority existing operator overrides (in the local ring) are preserved.
// Unpaired hosts are never migrated and never made to expose services.
// Once migrated, agent.json is rewritten to remove the retired flags while
// preserving unrelated JSON fields, making repeat migrations a clean no-op.
func migrateLegacyServices(cfgPath string, fc *conf.FileConfig, store *svcunit.Store) (bool, error) {
	if cfgPath == "" {
		return false, nil
	}
	if fc == nil {
		var err error
		fc, err = conf.LoadFile(cfgPath)
		if err != nil || fc == nil {
			return false, err
		}
	}
	// Do not make unpaired hosts expose services.
	// Only upgraded paired hosts should have legacy settings migrated.
	if (fc.AccessToken == "" && fc.Token == "") || fc.AgentName == "" {
		return false, nil
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	legacy, found := extractLegacyServiceConfig(data)
	if !found {
		// Fresh paired host or already migrated config — nothing to do.
		return false, nil
	}

	if store == nil {
		store = serviceStore()
	}

	var migratedAny bool
	if legacy.OllamaEnabled != nil {
		changed, err := migrateUnitOverride(store, "ollama", *legacy.OllamaEnabled)
		if err != nil {
			return false, fmt.Errorf("migrate ollama: %w", err)
		}
		if changed {
			migratedAny = true
		}
	}
	if legacy.OllamaPoolEnabled != nil {
		changed, err := migrateUnitOverride(store, "ollama-pool", *legacy.OllamaPoolEnabled)
		if err != nil {
			return false, fmt.Errorf("migrate ollama-pool: %w", err)
		}
		if changed {
			migratedAny = true
		}
	}
	if legacy.ShardEnabled != nil {
		changed, err := migrateUnitOverride(store, "shard", *legacy.ShardEnabled)
		if err != nil {
			return false, fmt.Errorf("migrate shard: %w", err)
		}
		if changed {
			migratedAny = true
		}
	}

	// Rewrite only the retired fields. Re-serializing FileConfig here would
	// discard unrelated fields written by a newer outpost or by an operator.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return migratedAny, fmt.Errorf("parse legacy config: %w", err)
	}
	delete(raw, "ollama_enabled")
	delete(raw, "ollama_pool_enabled")
	if shardRaw, ok := raw["shard"]; ok {
		var shard map[string]json.RawMessage
		if json.Unmarshal(shardRaw, &shard) == nil {
			delete(shard, "enabled")
			if len(shard) == 0 {
				delete(raw, "shard")
			} else {
				raw["shard"], _ = json.Marshal(shard)
			}
		} else {
			delete(raw, "shard")
		}
	}
	updated, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return migratedAny, fmt.Errorf("encode legacy config: %w", err)
	}
	if err := conf.SaveRawFile(cfgPath, updated); err != nil {
		return migratedAny, fmt.Errorf("resave config: %w", err)
	}

	if migratedAny {
		slog.Info("migrated legacy agent.json services into unit overrides")
	}

	return true, nil
}

func migrateUnitOverride(store *svcunit.Store, name string, on bool) (bool, error) {
	unit, ok := store.Unit(name)
	if !ok {
		return false, nil
	}
	// Preserve higher-priority existing operator service overrides.
	// An existing operator override lives in the local ring.
	if unit.Ring == assetring.RingLocal || unit.Ring.String() == "local" {
		return false, nil
	}
	after := unit
	after.Enabled = on
	if err := store.Save(after); err != nil {
		return false, err
	}
	return true, nil
}
