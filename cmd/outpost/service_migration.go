package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/qiangli/yoke/pkg/assetring"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

type legacyServiceConfig struct {
	OllamaEnabled     *bool
	OllamaPoolEnabled *bool
	ShardEnabled      *bool
	PodmanEnabled     *bool
	FilesEnabled      *bool
	OtelEnabled       *bool
	OtelPoolEnabled   *bool
	WarmEnabled       *bool
	WarmBudgetFrac    *float64
}

// legacyRetiredKeys are the top-level agent.json keys this migration owns and
// removes. files_signing_key and warm_desired are daemon state and stay.
var legacyRetiredKeys = []string{
	"ollama_enabled", "ollama_pool_enabled",
	"podman_enabled", "files_enabled",
	"otel_enabled", "otel_pool_enabled",
	"warm_serving_enabled", "warm_budget_frac",
}

func rawBool(raw map[string]json.RawMessage, key string, dst **bool) bool {
	v, ok := raw[key]
	if !ok {
		return false
	}
	var b *bool
	if err := json.Unmarshal(v, &b); err != nil || b == nil {
		return false
	}
	*dst = b
	return true
}

// extractLegacyServiceConfig inspects raw JSON for the retired agent.json
// keys that now live on service units.
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
	for key, dst := range map[string]**bool{
		"ollama_enabled":       &cfg.OllamaEnabled,
		"ollama_pool_enabled":  &cfg.OllamaPoolEnabled,
		"podman_enabled":       &cfg.PodmanEnabled,
		"files_enabled":        &cfg.FilesEnabled,
		"otel_enabled":         &cfg.OtelEnabled,
		"otel_pool_enabled":    &cfg.OtelPoolEnabled,
		"warm_serving_enabled": &cfg.WarmEnabled,
	} {
		if rawBool(raw, key, dst) {
			found = true
		}
	}
	if v, ok := raw["warm_budget_frac"]; ok {
		var f *float64
		if err := json.Unmarshal(v, &f); err == nil && f != nil {
			cfg.WarmBudgetFrac = f
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
	type step struct {
		unit     string
		enabled  *bool
		settings map[string]string
	}
	steps := []step{
		{unit: "ollama", enabled: legacy.OllamaEnabled},
		{unit: "ollama-pool", enabled: legacy.OllamaPoolEnabled},
		{unit: "shard", enabled: legacy.ShardEnabled},
		{unit: "podman", enabled: legacy.PodmanEnabled},
		{unit: "files", enabled: legacy.FilesEnabled},
		{unit: "otel", enabled: legacy.OtelEnabled, settings: boolSetting("pool", legacy.OtelPoolEnabled)},
		{unit: "warm", enabled: legacy.WarmEnabled, settings: floatSetting("budget_frac", legacy.WarmBudgetFrac)},
	}
	for _, st := range steps {
		if st.enabled == nil && len(st.settings) == 0 {
			continue
		}
		changed, err := migrateUnitOverride(store, st.unit, st.enabled, st.settings)
		if err != nil {
			return false, fmt.Errorf("migrate %s: %w", st.unit, err)
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
	for _, k := range legacyRetiredKeys {
		delete(raw, k)
	}
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

func boolSetting(key string, b *bool) map[string]string {
	if b == nil {
		return nil
	}
	return map[string]string{key: strconv.FormatBool(*b)}
}

func floatSetting(key string, f *float64) map[string]string {
	if f == nil || *f <= 0 {
		return nil
	}
	return map[string]string{key: strconv.FormatFloat(*f, 'g', -1, 64)}
}

// migrateUnitOverride writes the legacy choice as a unit override. enabled is
// nil when only settings were carried over, so a baseline default is never
// pinned by a key the operator never set.
func migrateUnitOverride(store *svcunit.Store, name string, enabled *bool, settings map[string]string) (bool, error) {
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
	if enabled != nil {
		after.Enabled = *enabled
	}
	if len(settings) > 0 {
		merged := make(map[string]string, len(unit.Settings)+len(settings))
		for k, v := range unit.Settings {
			merged[k] = v
		}
		for k, v := range settings {
			merged[k] = v
		}
		after.Settings = merged
	}
	if err := store.Save(after); err != nil {
		return false, err
	}
	return true, nil
}
