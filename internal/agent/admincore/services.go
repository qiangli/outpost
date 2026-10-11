package admincore

import (
	"context"
	"sort"
	"strconv"

	"github.com/qiangli/yoke/pkg/fleet"

	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// ServiceOps is the host's service-unit control surface as admincore uses it:
// the declarations and their desired state, the monitor's runtime state, and
// a nudge to republish exposure. One interface for every service — a seeded
// built-in, an in-process outpost capability, a third-party YAML — because
// the whole point of the unit model is that outpost stops knowing which is
// which.
//
// Implemented by *svcunit.Store (plus the daemon's Reconciler for Kick); nil
// on test paths and on a host whose unit registry could not be opened, where
// the service operations report "not configured" instead of panicking.
type ServiceOps interface {
	// All lists every declared unit.
	All() ([]fleet.Service, error)
	// Unit resolves one declaration.
	Unit(name string) (fleet.Service, bool)
	// SetEnabled persists desired state — the `bashy service enable|disable`
	// write.
	SetEnabled(name string, on bool) error
	// SetFields applies dotted-path overrides — the `bashy service set` write.
	SetFields(ctx context.Context, name string, fields map[string]string) error
	// Snapshot is the monitor's state plus what outpost currently publishes.
	Snapshot() svcunit.Snapshot
	// Kick asks the exposure reconciler to republish now, which is what makes
	// a toggle apply without an outpost restart.
	Kick()
}

// ServiceUnitView is one unit as the admin UI, MCP and `outpost status` see
// it: the declaration's operator-visible surface joined with the monitor's
// state. It replaces the per-service `*_enabled` / `*_port` rows that used to
// be hand-maintained in three places for every new daemon.
type ServiceUnitView struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Provider    string   `json:"provider"`
	Enabled     bool     `json:"enabled"`
	State       string   `json:"state,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	PID         int      `json:"pid,omitempty"`
	Restarts    int      `json:"restarts,omitempty"`
	Health      string   `json:"health,omitempty"`
	Ports       []string `json:"ports,omitempty"`
	App         string   `json:"app,omitempty"`
	Mesh        []string `json:"mesh,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	Publishing  []string `json:"publishing,omitempty"`
	Ring        string   `json:"ring,omitempty"`
	Capability  string   `json:"capability,omitempty"`
	Owner       string   `json:"owner,omitempty"`
}

// ServiceStatusView is the whole host's service picture.
type ServiceStatusView struct {
	// MonitorReachable reports whether `bashy service status` answered. False
	// means the rows carry declared state and the last state seen, which the
	// UI must say out loud rather than presenting as live truth.
	MonitorReachable bool              `json:"monitor_reachable"`
	Units            []ServiceUnitView `json:"units,omitempty"`
}

// ServiceStatus is the read surface the admin UI and MCP render. It is the
// structured replacement for grepping a service's text output, and for the
// never-wired apphealth prober.
func (s *Server) ServiceStatus() (ServiceStatusView, error) {
	ops := s.deps.Services
	if ops == nil {
		return ServiceStatusView{}, unavailable("the service-unit registry is not available on this host")
	}
	units, err := ops.All()
	if err != nil {
		return ServiceStatusView{}, internalErr("%s", err.Error())
	}
	snap := ops.Snapshot()
	byUnit := make(map[string]svcunit.UnitState, len(snap.Units))
	for _, st := range snap.Units {
		byUnit[st.Unit] = st
	}
	out := ServiceStatusView{MonitorReachable: snap.MonitorReachable}
	for _, u := range units {
		if u.IsRetired() {
			continue
		}
		row := ServiceUnitView{
			Name: u.Name, Description: u.Description, Provider: u.ProviderName(),
			Enabled: u.Enabled, Requires: u.Dependencies(), Ring: u.Ring.String(),
			Capability: u.Capability, Owner: u.Owner,
		}
		for _, p := range u.Ports {
			row.Ports = append(row.Ports, bindLabel(p.Bind)+":"+strconv.Itoa(p.Port))
		}
		if a := u.Expose.App; a != nil {
			row.App = a.Name
			if row.App == "" {
				row.App = u.Name
			}
		}
		for _, m := range u.Expose.Mesh {
			row.Mesh = append(row.Mesh, m.Name)
		}
		if st, ok := byUnit[u.Name]; ok {
			row.State, row.Reason, row.PID = string(st.State), st.Reason, st.PID
			row.Restarts, row.Health, row.Publishing = st.Restarts, st.Health, st.Publishing
		}
		out.Units = append(out.Units, row)
	}
	sort.Slice(out.Units, func(i, j int) bool { return out.Units[i].Name < out.Units[j].Name })
	return out, nil
}

func bindLabel(b string) string {
	if b == "" {
		return "loopback"
	}
	return b
}

// applyServiceDesiredState writes the desired state and declaration
// overrides a BuiltinsParams carried, then asks the exposure reconciler to
// republish. It reports whether anything was written.
//
// There is no per-service branch and no restart: the unit store is the single
// source of desired state, the running monitor reconciles the processes, and
// Kick republishes outpost's tunnel/mesh exposure. This is what retires the
// old "SetBuiltins returns RestartPending while the CLI claims a restart"
// mismatch for services.
func (s *Server) applyServiceDesiredState(ctx context.Context, p BuiltinsParams) (bool, error) {
	if len(p.Services) == 0 && len(p.ServiceSettings) == 0 {
		return false, nil
	}
	ops := s.deps.Services
	if ops == nil {
		return false, unavailable("the service-unit registry is not available on this host")
	}
	// Settings first: a unit being switched on in the same call should come
	// up with the declaration the operator just gave it, not the old one.
	for _, name := range sortedKeys(p.ServiceSettings) {
		if err := ops.SetFields(ctx, name, p.ServiceSettings[name]); err != nil {
			return false, serviceWriteError(name, err)
		}
	}
	for _, name := range sortedBoolKeys(p.Services) {
		if err := ops.SetEnabled(name, p.Services[name]); err != nil {
			return false, serviceWriteError(name, err)
		}
	}
	ops.Kick()
	return true, nil
}

// serviceWriteError maps a unit-store refusal onto the HTTP/MCP error shape.
// An unknown unit and a port conflict are both operator mistakes the caller
// can fix, so they are 400s carrying fleet's own message — which already
// names the holder of a contested port.
func serviceWriteError(name string, err error) error {
	var unknown *svcunit.UnknownUnitError
	if asError(err, &unknown) {
		return badRequest("%s", unknown.Error())
	}
	return badRequest("service %q: %s", name, err.Error())
}

// asError is errors.As for a concrete pointer target without pulling the
// generic reflection path into every call site.
func asError[T error](err error, target *T) bool {
	for err != nil {
		if t, ok := err.(T); ok {
			*target = t
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string { return sortedKeys(m) }

// serviceUnitViews is the SafeView projection: declarations only, since
// SafeView is a pure function of the persisted config plus live closures.
func serviceUnitViews(ops ServiceOps) []ServiceUnitView {
	if ops == nil {
		return nil
	}
	units, err := ops.All()
	if err != nil {
		return nil
	}
	out := make([]ServiceUnitView, 0, len(units))
	for _, u := range units {
		if u.IsRetired() {
			continue
		}
		row := ServiceUnitView{
			Name: u.Name, Description: u.Description, Provider: u.ProviderName(),
			Enabled: u.Enabled, Requires: u.Dependencies(), Ring: u.Ring.String(),
			Capability: u.Capability, Owner: u.Owner,
		}
		for _, p := range u.Ports {
			row.Ports = append(row.Ports, bindLabel(p.Bind)+":"+strconv.Itoa(p.Port))
		}
		if a := u.Expose.App; a != nil {
			row.App = a.Name
			if row.App == "" {
				row.App = u.Name
			}
		}
		for _, m := range u.Expose.Mesh {
			row.Mesh = append(row.Mesh, m.Name)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
