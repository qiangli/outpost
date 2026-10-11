package svcunit

import (
	"context"
	"fmt"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// Capability is the generic adapter an in-process outpost subsystem registers
// itself through. It is the whole of the "internal provider" wiring: a
// subsystem supplies four closures at its existing boot site and gains the
// full service contract — declaration, desired state, live reconfiguration,
// health, structured status, state events and app/mesh exposure — with no
// code in this package, in the monitor or in the toggle surfaces that knows
// the subsystem exists.
//
// Deliberately NOT a process: an internal capability has no child and no pid
// (service.Status.PID stays 0). Wrapping one in a fake executable so the
// monitor could spawn it would make the status lie about what is running.
type Capability struct {
	// ActivateFn turns the capability on. cfg carries the declaration's
	// settings, resolved env and declared ports.
	ActivateFn func(ctx context.Context, cfg service.Config) error
	// ConfigureFn applies a changed declaration to a live capability. Return
	// service.ErrRestartRequired to have the reconciler cycle it instead.
	// Nil means "cannot apply live": every change becomes a cycle.
	ConfigureFn func(ctx context.Context, cfg service.Config) error
	// DeactivateFn turns it off. Must be idempotent.
	DeactivateFn func(ctx context.Context) error
	// HealthFn reports liveness. Nil means "activated implies healthy".
	HealthFn func(ctx context.Context) error
}

// Activate implements service.Capability.
func (c Capability) Activate(ctx context.Context, cfg service.Config) error {
	if c.ActivateFn == nil {
		return fmt.Errorf("svcunit: capability for unit %q has no activate", cfg.Unit)
	}
	return c.ActivateFn(ctx, cfg)
}

// Configure implements service.Capability.
func (c Capability) Configure(ctx context.Context, cfg service.Config) error {
	if c.ConfigureFn == nil {
		return service.ErrRestartRequired
	}
	return c.ConfigureFn(ctx, cfg)
}

// Deactivate implements service.Capability.
func (c Capability) Deactivate(ctx context.Context) error {
	if c.DeactivateFn == nil {
		return nil
	}
	return c.DeactivateFn(ctx)
}

// Health implements service.Capability.
func (c Capability) Health(ctx context.Context) error {
	if c.HealthFn == nil {
		return nil
	}
	return c.HealthFn(ctx)
}

// Hosted builds the capability registry and the internal provider for the
// outpost daemon. caps is id -> capability, where the id is what a unit's
// `capability:` field names (outpost.files, outpost.registry, …).
func Hosted(caps map[string]service.Capability) (*service.Internal, error) {
	reg := service.NewRegistry()
	for id, c := range caps {
		if err := reg.Register(id, c); err != nil {
			return nil, err
		}
	}
	return service.NewInternal(fleet.ServiceOwnerOutpost, reg), nil
}
