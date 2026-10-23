package main

import (
	"github.com/qiangli/yoke/pkg/service"
)

// Capability ids — the cross-repo contract between a unit declaration's
// `capability:` field and the subsystem this daemon hosts for it
// (docs/settings.md, "Contract notes for the unit seeds"). The id is the
// stable name; a unit's NAME is the operator's, so nothing here keys off it.
const (
	capFiles        = "outpost.files"
	capPodman       = "outpost.podman"
	capSandbox      = "outpost.sandbox"
	capOllama       = "outpost.ollama"
	capOllamaPool   = "outpost.ollama-pool"
	capWarm         = "outpost.warm"
	capShard        = "outpost.shard"
	capLANInference = "outpost.lan-inference"
	capOtel         = "outpost.otel"
	capYcode        = "outpost.ycode"
	capActrunner    = "outpost.actrunner"
	capSSHD         = "outpost.sshd"
)

// capabilityDeps is the set of in-process capabilities the daemon
// constructed, plus the values only the daemon can resolve.
//
// Each field is nil-tolerant: a capability the daemon could not build (no
// app registry on a client-only host, no mesh for sharding) is simply not
// registered, and its unit then reports "capability is not registered"
// rather than silently doing nothing.
type capabilityDeps struct {
	// Forge resolves a local git-forge unit's loopback URL (localForgeURL),
	// the default instance for a CI runner that declares none.
	Forge func(unit string) string

	Files        *filesCapability
	Podman       *podmanCapability
	Sandbox      *sandboxCapability
	Ollama       *ollamaCapability
	OllamaPool   *ollamaPoolCapability
	Warm         *warmCapability
	Shard        *shardCapability
	LANInference *lanInferenceCapability
	Otel         *otelCapability
	Ycode        *ycodeCapability
	SSHD         *sshdCapability
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
// all. A capability is warranted when activation needs something only THIS
// process can do, and every entry below clears that bar for a stated
// reason:
//
//   - files        — the handler is compiled into this binary.
//   - podman       — the socket is detected here and is platform-dependent.
//   - sandbox      — same socket, plus an in-process filter, capacity
//     intercept and image prewarmer wrapping one app route.
//   - ollama       — the daemon URL is detected here; the mount carries the
//     `llm` descriptor, the in-flight counter and a capacity intercept.
//   - ollama-pool  — pushes this host's registry using the SAME counter the
//     proxy's capacity probe reports, so the two cannot disagree.
//   - warm         — drives in-process model residency off a load sensor
//     shared with cluster placement.
//   - shard        — rides this process's libp2p forwarder and peer plane.
//   - lan-inference— an in-process reverse proxy on a declared LAN port.
//   - otel / ycode — the proxy URL and bearer come from ycode's own
//     manifest, and each surface needs a bearer-injecting proxy wrap.
//   - actrunner    — a provisioning handshake with no declarable hook; see
//     capability_actrunner.go.
//   - sshd         — the LAN TCP listener is a socket this process must
//     accept on; see capability_sshd.go.
func outpostCapabilities(deps capabilityDeps) map[string]service.Capability {
	caps := map[string]service.Capability{}
	add := func(id string, c service.Capability, ok bool) {
		if ok {
			caps[id] = c
		}
	}
	add(capFiles, deps.Files, deps.Files != nil)
	add(capPodman, deps.Podman, deps.Podman != nil)
	add(capSandbox, deps.Sandbox, deps.Sandbox != nil)
	add(capOllama, deps.Ollama, deps.Ollama != nil)
	add(capOllamaPool, deps.OllamaPool, deps.OllamaPool != nil)
	add(capWarm, deps.Warm, deps.Warm != nil)
	add(capShard, deps.Shard, deps.Shard != nil)
	add(capLANInference, deps.LANInference, deps.LANInference != nil)
	add(capOtel, deps.Otel, deps.Otel != nil)
	add(capYcode, deps.Ycode, deps.Ycode != nil)
	add(capSSHD, deps.SSHD, deps.SSHD != nil)

	ci := &actrunnerCapability{
		Forge:      deps.Forge,
		VaultEnv:   loadVaultSecretsEnv,
		DockerHost: resolveActrunnerDockerHost,
	}
	caps[capActrunner] = ci
	return caps
}
