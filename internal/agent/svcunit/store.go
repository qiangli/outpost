package svcunit

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// Store is the unit registry as outpost reads and writes it.
//
// Declarations and desired state live in fleet's registry rings (embedded
// seeds -> shared dirs -> the host-local store), and fleet is a Go library.
// So outpost talks to the SAME store `bashy service` does, in process,
// through the same validation — rather than shelling out to parse CLI output.
// `outpost builtins set --meet=on` and `bashy service enable meet` are then
// not two code paths that must agree: they are one write.
//
// Only one thing genuinely needs the monitor: the runtime state of a PROCESS
// unit, which only the process that owns the child can report. That is what
// Monitor is for.
type Store struct {
	// Catalog is the registry. Nil uses fleet.New() — the host's real store.
	Catalog *fleet.Catalog
	// Monitor reports process units' runtime state. Nil (or a failing one)
	// means outpost has no monitor verdict; see Reconciler.effectiveState.
	Monitor interface {
		Statuses(ctx context.Context) ([]service.Status, error)
	}
}

func (s *Store) catalog() *fleet.Catalog {
	if s != nil && s.Catalog != nil {
		return s.Catalog
	}
	return fleet.New()
}

// All returns every declared unit, parse failures reported rather than
// hidden: a unit outpost cannot read is a unit it must not silently drop.
func (s *Store) All() ([]fleet.Service, error) {
	units, errs := s.catalog().Services()
	return units, errors.Join(errs...)
}

// Units implements UnitSource: the units outpost has work for — one with a
// declared app/mesh exposure, or an internal capability this process hosts.
// The filter reads the DECLARATION, never the name.
func (s *Store) Units(context.Context) ([]fleet.Service, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	out := make([]fleet.Service, 0, len(all))
	for _, u := range all {
		if u.IsRetired() {
			continue
		}
		if u.Expose.App != nil || len(u.Expose.Mesh) > 0 || u.ProviderName() == fleet.ServiceProviderInternal {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Statuses implements UnitSource by asking the monitor.
func (s *Store) Statuses(ctx context.Context) ([]service.Status, error) {
	if s == nil || s.Monitor == nil {
		return nil, fmt.Errorf("svcunit: no service monitor configured")
	}
	return s.Monitor.Statuses(ctx)
}

// SetEnabled persists a unit's desired state. This is the single write path
// every outpost toggle surface takes — `builtins set`, the MCP tool and the
// admin UI — and it is byte-for-byte the write `bashy service enable|disable`
// performs: the same sparse local overlay, validated by the same rules
// (including the port-conflict check against every other enabled unit).
//
// Nothing is started or stopped here. The running monitor reconciles the new
// desired state live, and the caller's Reconciler.Kick republishes exposure,
// which is why a toggle no longer needs an outpost restart.
func (s *Store) SetEnabled(name string, on bool) error {
	cat := s.catalog()
	before, ok := cat.Service(name)
	if !ok {
		return &UnknownUnitError{Name: name, Known: s.names()}
	}
	if before.Enabled == on {
		return nil
	}
	after := before
	after.Enabled = on
	return cat.SaveService(after)
}

// Save persists a whole declaration (a `bashy service set` equivalent for
// callers that already hold the record).
func (s *Store) Save(u fleet.Service) error { return s.catalog().SaveService(u) }

// Unit resolves one declaration.
func (s *Store) Unit(name string) (fleet.Service, bool) { return s.catalog().Service(name) }

func (s *Store) names() []string {
	all, _ := s.All()
	out := make([]string, 0, len(all))
	for _, u := range all {
		out = append(out, u.Name)
	}
	return out
}

// UnknownUnitError names a unit that is not declared, and what is — so a
// toggle typo is answerable without a second command.
type UnknownUnitError struct {
	Name  string
	Known []string
}

func (e *UnknownUnitError) Error() string {
	return fmt.Sprintf("no service unit %q is declared (declared: %v); add one with `bashy service add`", e.Name, e.Known)
}
