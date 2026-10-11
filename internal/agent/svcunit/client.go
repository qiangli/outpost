// Package svcunit is outpost's side of the one service model
// (docs/bashy-service-design.md, Sprint 418 S15/S16/S18).
//
// The split it implements:
//
//   - `bashy service` owns the unit DECLARATIONS (fleet.Service) and their
//     desired state. S15.
//   - `bashy supervisord` is the service monitor: it keeps enabled units up
//     and publishes their runtime state (service.Status). S16.
//   - `outpost supervisord` is init: it starts and watches exactly the
//     outpost daemon and the bashy monitor, never an individual unit.
//   - the outpost DAEMON — this package — reads declarations + monitor state
//     and reconciles two things it alone owns: matrix-tunnel app routes
//     (AppRegistry) and mesh exposures (mesh Forwarder). It also hosts the
//     INTERNAL provider for units whose capability lives in this process.
//
// Nothing in this package branches on a unit's name. A seeded built-in, an
// internal outpost capability and a third-party YAML a user just dropped in
// all travel the same code: declaration in, provider + exposure out.
package svcunit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// Runner executes one `bashy …` invocation and returns its stdout. It is the
// single seam this package has on the outside world, so every test drives the
// real reconciler against a scripted bashy.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// ExecRunner builds a Runner that shells out to the bashy binary resolve
// returns. The binary is resolved per call rather than cached: outpost
// self-heals a missing bashy and reconciles it to the fleet-matched version
// while running, so a cached path can go stale under us.
func ExecRunner(resolve func(context.Context) (string, error)) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		bin, err := resolve(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve bashy: %w", err)
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		out, err := cmd.Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && len(ee.Stderr) > 0 {
				return out, fmt.Errorf("bashy %s: %w: %s", strings.Join(args, " "), err,
					strings.TrimSpace(string(ee.Stderr)))
			}
			return out, fmt.Errorf("bashy %s: %w", strings.Join(args, " "), err)
		}
		return out, nil
	}
}

// Client is the typed view of the `bashy service` surface. Declarations come
// from `service list --json` (S15) and runtime state from
// `service status --json` (S16) — the monitor's local status socket, reached
// through its own CLI so outpost never has to know the socket's wire format.
type Client struct {
	Run Runner
	// Timeout bounds one bashy invocation. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds one `bashy service …` call. Generous enough for a
// cold binmgr-provisioned bashy on a slow disk, short enough that a wedged
// monitor cannot stall the exposure reconciler for long.
const DefaultTimeout = 30 * time.Second

func (c *Client) call(ctx context.Context, args ...string) ([]byte, error) {
	if c == nil || c.Run == nil {
		return nil, fmt.Errorf("svcunit: no bashy runner configured")
	}
	to := c.Timeout
	if to <= 0 {
		to = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	return c.Run(ctx, args...)
}

// listEnvelope is the shape fleet's --json list writers emit: a kind-tagged
// envelope around the rows. Only the rows are decoded here.
type listEnvelope[T any] struct {
	Items []T `json:"items"`
}

// decodeRows accepts either the envelope fleet writes (`{"items":[…]}`) or a
// bare JSON array, so a future change of the list writer's envelope does not
// silently blank outpost's exposure.
func decodeRows[T any](data []byte) ([]T, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var rows []T
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	var env listEnvelope[T]
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

// Units returns every declared unit that outpost has anything to do with:
// one that declares an app/mesh exposure, or one whose provider is internal
// (a capability this process may host). `service list --json` is a
// projection, so the full declaration of each selected row is read back; the
// selection keeps that from being one subprocess per unit on every pass, and
// it is a property of the DECLARATION, never of the unit's name.
//
// Declarations are deliberately re-read only on boot and after a write (see
// Reconciler.Kick) — the monitor's status, polled far more often, is the
// cheap call.
func (c *Client) Units(ctx context.Context) ([]fleet.Service, error) {
	out, err := c.call(ctx, "service", "list", "--json")
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows[unitRow](out)
	if err != nil {
		return nil, fmt.Errorf("svcunit: decode service list: %w", err)
	}
	units := make([]fleet.Service, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.Name) == "" || !outpostCares(r) {
			continue
		}
		u, err := c.Unit(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units, nil
}

// unitRow is the subset of fleet's service list row this package reads.
type unitRow struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	App      string   `json:"app"`
	Mesh     []string `json:"mesh"`
}

// outpostCares selects the units the outpost daemon has work for: an
// exposure to publish, or an internal capability it may host.
func outpostCares(r unitRow) bool {
	return r.App != "" || len(r.Mesh) > 0 || r.Provider == fleet.ServiceProviderInternal
}

// Unit reads one unit's full declaration.
func (c *Client) Unit(ctx context.Context, name string) (fleet.Service, error) {
	out, err := c.call(ctx, "service", "show", name, "--json")
	if err != nil {
		return fleet.Service{}, err
	}
	var u fleet.Service
	if err := json.Unmarshal(out, &u); err != nil {
		return fleet.Service{}, fmt.Errorf("svcunit: decode service show %s: %w", name, err)
	}
	if u.Name == "" {
		u.Name = name
	}
	return u, nil
}

// Statuses returns the monitor's runtime state for every unit. An error here
// means the monitor could not be reached; the caller decides what an unknown
// state implies (see Reconciler.degrade).
func (c *Client) Statuses(ctx context.Context) ([]service.Status, error) {
	out, err := c.call(ctx, "service", "status", "--json")
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows[service.Status](out)
	if err != nil {
		return nil, fmt.Errorf("svcunit: decode service status: %w", err)
	}
	return rows, nil
}

// SetEnabled persists a unit's desired state. The running monitor reconciles
// it live — this is the ONLY path outpost's toggles take, so `builtins set`,
// the MCP tool and the admin UI can no longer diverge from the unit store.
func (c *Client) SetEnabled(ctx context.Context, name string, on bool) error {
	verb := "disable"
	if on {
		verb = "enable"
	}
	_, err := c.call(ctx, "service", verb, name)
	return err
}

// Set applies sparse field overrides to a unit (`bashy service set NAME --set
// k=v …`). Keys are dotted declaration paths, e.g. ports.0.port.
func (c *Client) Set(ctx context.Context, name string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	args := []string{"service", "set", name}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--set", k+"="+fields[k])
	}
	_, err := c.call(ctx, args...)
	return err
}
