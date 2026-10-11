package main

import (
	"github.com/qiangli/yoke/pkg/service"
)

// capabilityDeps is what the in-process capabilities need from the daemon's
// wiring: values resolved on this host that a declaration cannot carry.
type capabilityDeps struct {
	// Forge resolves a local git-forge unit's loopback URL (localForgeURL),
	// the default instance for a CI runner that declares none.
	Forge func(unit string) string
}

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
// The bar for being here is narrow, and most wrapped tools do NOT clear it:
// Zot, SeaweedFS, Kopia and the Headlamp loopback forward are each a
// command, a port and a data dir, which `provider: process` expresses
// completely — so the monitor owns their children and outpost owns only
// their publication (`expose.app` / `expose.mesh`), with no code here at
// all. A capability is warranted when activation needs something only this
// process can do: see capability_actrunner.go for the one case and why.
//
// The subsystems still carrying their own boot wiring — the File Browser
// mount, the container sandbox, the podman/ollama proxies — move here the
// same way: a declaration (yoke S17) plus one entry below.
func outpostCapabilities(deps capabilityDeps) map[string]service.Capability {
	ci := &actrunnerCapability{
		Forge:      deps.Forge,
		VaultEnv:   loadVaultSecretsEnv,
		DockerHost: resolveActrunnerDockerHost,
	}
	return map[string]service.Capability{
		// Canonical id. The alias is the name the capability's role would
		// suggest, registered so a unit seed written against either spelling
		// binds instead of failing with "capability is not registered" —
		// the ids are a cross-repo contract (docs/settings.md, "Contract
		// notes for the unit seeds") and outpost answers to both halves of
		// it rather than to whichever one it guessed.
		"outpost.actrunner": ci,
		"outpost.ci":        ci,
	}
}
