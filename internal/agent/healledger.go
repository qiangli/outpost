package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// UpdateHeal serializes read/append transactions across daemon and CLI processes.
// A crashed writer leaves a lock directory; fail closed rather than steal a
// lock from a possibly live repair. Operators may remove it after stopping both.
func UpdateHeal(path, id string, update func(*HealRecord) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock := path + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(lock, 0700)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("heal ledger busy; check writer before removing lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer os.Remove(lock)
	rows, err := ReadHealRecords(path)
	if err != nil {
		return err
	}
	row := HealRecord{ID: id}
	for _, r := range rows {
		if r.ID == id {
			row = r
			break
		}
	}
	before, _ := json.Marshal(row)
	if err := update(&row); err != nil {
		return err
	}
	after, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if string(before) == string(after) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := restrictSecretFile(path); err != nil {
		return err
	}
	if _, err := f.Write(append(after, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
