// Package sandbox implements outpost's safe-by-default container
// "sandbox" provider: a filtered libpod/docker API proxy that strips the
// escape-bearing knobs (privileged, host namespaces, host bind-mounts,
// added capabilities, devices) and injects per-request resource caps, so
// a remote caller who clears the cloudbox elevation gate can run
// containers without getting root-equivalent control of the host.
//
// It mirrors internal/agent/ollama: a small Service glues the in-flight
// Counter to the two outpost-side surfaces it feeds — the proxy-wrap
// middleware (live load tracking) and the /_pool/capacity intercept
// (cloudbox-side scheduling queries). The companion filter.go is the
// security core; policy.go holds the operator-tunable limits.
//
// This is deliberately distinct from the raw /app/podman/ passthrough
// (which stays admin-only, for trusted self-use): the sandbox mount is
// what a thin client or an untrusted tenant talks to.
package sandbox

// CapabilityType is the AppCapabilities.Type value outpost advertises for
// the sandbox mount via GET /apps, so cloudbox can discover sandbox-
// bearing hosts without a separate probe — the same mechanism the ollama
// builtin uses to advertise {type:"llm"}.
const CapabilityType = "sandbox"

// CapacityReport is the JSON body served at /app/sandbox/_pool/capacity
// and (later) pushed to cloudbox's sandbox registry. It is the analog of
// ollama.CapacityReport: cloudbox's router reads it to pick the warmest /
// least-loaded host when distributing a sandbox request across the fleet.
//
// Version is bumped when the shape changes so cloudbox can decode old and
// new payloads. PoolWarm / PoolWarming stay zero until the Phase-A warm
// pool lands; they are part of the wire shape now so adding the pool
// later needs no schema change.
type CapacityReport struct {
	Version int `json:"version"`
	// MaxContainers is the policy ceiling on concurrent sandbox
	// containers this host will run. Zero means "unset / no explicit
	// ceiling" — cloudbox treats zero as "use the host's own judgement",
	// never as "cannot run anything", mirroring the ollama zero-as-
	// default convention.
	MaxContainers int `json:"max_containers"`
	// InFlight is the number of sandbox create/exec requests currently
	// being served through this mount — a live load proxy until the warm
	// pool tracks real running-container counts.
	InFlight int `json:"in_flight"`
	// PoolWarm / PoolWarming are the pre-warmed-container depth and the
	// number currently being replenished. Reserved for the Phase-A warm
	// pool; zero until then.
	PoolWarm    int `json:"pool_warm"`
	PoolWarming int `json:"pool_warming"`
	// Isolation names the OCI runtime tier this host enforces:
	// "runc" (shared kernel, the Phase-A default), "gvisor", or "kata".
	// cloudbox routes untrusted work only to hosts advertising a
	// VM/sandbox-grade runtime; empty is treated as "runc".
	Isolation string `json:"isolation,omitempty"`
}
