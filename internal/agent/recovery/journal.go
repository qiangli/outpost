// Package recovery records a bounded local history of Outpost reachability transitions.
package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/outpost/internal/agent/conf"
)

const (
	FileName  = "recovery.json"
	MaxEvents = 64
	MaxBytes  = 16 * 1024
)

type Event struct {
	At        time.Time `json:"at"`
	Component string    `json:"component"`
	State     string    `json:"state"`
	Reason    string    `json:"reason"`
}

var code = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// Record accepts event codes only. In particular, caller errors and
// authorization replies belong outside this journal.
func Record(component, state, reason string) {
	if err := appendEvent(Event{time.Now().UTC(), component, state, reason}); err != nil {
		slog.Warn("outpost recovery event could not be saved")
	}
}

func appendEvent(event Event) error {
	if !code.MatchString(event.Component) || !code.MatchString(event.State) || !code.MatchString(event.Reason) {
		return errors.New("invalid recovery event code")
	}
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	lock, err := lockfile.AcquireWithin(path+".lock", 200*time.Millisecond, lockfile.Holder{Intent: "outpost recovery journal"})
	if err != nil {
		return err
	}
	defer lock.Release()
	events, err := read(path)
	if err != nil {
		return err
	}
	events = append(events, event)
	if len(events) > MaxEvents {
		events = events[len(events)-MaxEvents:]
	}
	data, err := json.Marshal(events)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxBytes {
		return fmt.Errorf("recovery journal exceeded %d bytes", MaxBytes)
	}
	temp := path + ".tmp"
	defer os.Remove(temp)
	if err := os.WriteFile(temp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func Read() ([]Event, error) {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return nil, err
	}
	return read(filepath.Join(dir, FileName))
}

func read(path string) ([]Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("recovery journal exceeded %d bytes", MaxBytes)
	}
	events := []Event{}
	err = json.Unmarshal(data, &events)
	return events, err
}
