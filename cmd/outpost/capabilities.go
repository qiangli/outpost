package main

import (
	"github.com/qiangli/yoke/pkg/service"
)

// outpostCapabilities is the set of in-process capabilities this daemon
// hosts, keyed by the id a unit's `capability:` field names.
//
// This is the registration point the "internal provider" contract asks for
// (docs/bashy-service-design.md, "Rod not fish service contract"). A
// subsystem that wants its configuration, enablement, health, status and
// exposure to come from a unit declaration adds ONE entry here, built from
// svcunit.Capability's four closures. There is no per-service supervisor, no
// per-service toggle, no per-service exposure code and no feature flag
// beside the unit's `enabled` bit — and no fake child process or synthetic
// pid, because an in-process capability has neither.
//
// The map is empty today on purpose: the capability ids the seeded units
// name (meet.events is owned by meet, not outpost) do not yet include an
// outpost-hosted one, and the subsystems that would move here — the File
// Browser mount, the container sandbox, the podman/ollama proxies, Zot,
// SeaweedFS, Kopia, the Headlamp forward — keep their current boot wiring
// until S17 seeds their declarations. Moving one is then a declaration plus
// an entry here, with nothing else in outpost to change; see
// cmd/outpost/capabilities_test.go for the shape and the contract it must
// satisfy.
func outpostCapabilities() map[string]service.Capability {
	return map[string]service.Capability{}
}
