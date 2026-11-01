package admincore

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

func (s *Server) healLedgerPath() string {
	return filepath.Join(filepath.Dir(s.deps.ConfigPath), "heals.jsonl")
}

// ListHeals returns the latest state of local app and upgrade heal records.
func (s *Server) ListHeals() ([]agent.HealRecord, error) {
	return agent.ReadHealRecords(s.healLedgerPath())
}

// UndoHeal restores the exact prior app configuration through UpsertApp.
func (s *Server) UndoHeal(id string) (conf.AppConfig, error) {
	rows, err := s.ListHeals()
	if err != nil {
		return conf.AppConfig{}, err
	}
	for _, row := range rows {
		if row.ID != id {
			continue
		}
		if row.Rung != "address" || row.Result != "healed" {
			return conf.AppConfig{}, badRequest("heal is not a reversible app configuration change")
		}
		b, err := json.Marshal(row.Before)
		if err != nil {
			return conf.AppConfig{}, err
		}
		var before conf.AppConfig
		if err := json.Unmarshal(b, &before); err != nil {
			return conf.AppConfig{}, err
		}
		if before.Name != row.App {
			return conf.AppConfig{}, fmt.Errorf("heal ledger app mismatch")
		}
		return s.UpsertApp(AppUpsertParams{AppConfig: before})
	}
	return conf.AppConfig{}, notFound("heal record not found")
}
