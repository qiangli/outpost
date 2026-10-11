// Package svcunit is outpost's side of the one service model
// (docs/bashy-service-design.md, Sprint 418 S15/S16/S18).
//
// The split it implements:
//
//   - `bashy service` owns the unit DECLARATIONS (fleet.Service) and their
//     desired state, in fleet's registry rings. S15.
//   - `bashy supervisord` is the service monitor: it keeps enabled units up
//     and publishes their runtime state (service.Status). S16.
//   - `outpost supervisord` is init: it starts and watches exactly the
//     outpost daemon and the bashy monitor, never an individual unit.
//   - the outpost DAEMON — this package — reads declarations and monitor
//     state and reconciles the two things it alone owns: matrix-tunnel app
//     routes (AppRegistry) and mesh exposures (mesh Forwarder). It also
//     hosts the INTERNAL provider for units whose capability lives in this
//     process.
//
// Declarations are read and written IN PROCESS through fleet's Go API (see
// Store), because that is the same store and the same validation
// `bashy service` uses — one write, not two code paths that have to agree.
// Only a process unit's runtime state needs the monitor, and that is this
// file: the monitor's status, reached through its own CLI so outpost never
// has to know the status socket's wire format.
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
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/service"
)

// Runner executes one `bashy …` invocation and returns its stdout. It is the
// single seam this package has on the outside world, so a test can drive the
// real reconciler against a scripted monitor.
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
		out, err := exec.CommandContext(ctx, bin, args...).Output()
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

// Monitor is the typed view of the service monitor's status surface
// (`bashy service status --json`, S16). It is the ONLY thing outpost cannot
// answer locally: a process unit's live state belongs to whoever owns the
// child.
type Monitor struct {
	Run Runner
	// Timeout bounds one invocation. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds one status call. Generous enough for a cold
// binmgr-provisioned bashy on a slow disk, short enough that a wedged
// monitor cannot stall the exposure reconciler for long.
const DefaultTimeout = 15 * time.Second

// Statuses returns the monitor's runtime state for every unit. An error means
// the monitor could not be reached — which is NOT the same as "every unit is
// down"; see Reconciler.effectiveState for what outpost does with that.
func (m *Monitor) Statuses(ctx context.Context) ([]service.Status, error) {
	if m == nil || m.Run == nil {
		return nil, fmt.Errorf("svcunit: no service monitor configured")
	}
	to := m.Timeout
	if to <= 0 {
		to = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	out, err := m.Run(ctx, "service", "status", "--json")
	if err != nil {
		return nil, err
	}
	rows, err := decodeRows[service.Status](out)
	if err != nil {
		return nil, fmt.Errorf("svcunit: decode service status: %w", err)
	}
	return rows, nil
}

// decodeRows accepts either the kind-tagged envelope fleet's --json writers
// emit (`{"items":[…]}`) or a bare JSON array, so a change of the list
// envelope cannot silently blank outpost's view of the host.
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
	var env struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}
