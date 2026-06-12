package sandbox

import "strings"

// Policy is the operator-tunable posture for the sandbox mount. Zero
// values mean "no explicit limit" for the resource caps (the filter then
// leaves the caller's value — or the daemon default — untouched) and
// "deny" for the escape knobs (those are not tunable in Phase A: the
// whole point of the sandbox mount is that they are always off).
//
// Loaded from FileConfig at boot; passed by value into NewService.
type Policy struct {
	// MaxMemoryBytes, when > 0, is injected as the per-container memory
	// limit if the create request didn't set one. A request asking for
	// MORE than this is clamped down to it.
	MaxMemoryBytes int64
	// NanoCPUs, when > 0, is the per-container CPU cap in docker
	// "NanoCpus" units (1e9 == 1 CPU). Injected/clamped like memory.
	NanoCPUs int64
	// PidsLimit, when > 0, caps the number of processes per container
	// (fork-bomb defense). Injected/clamped like memory.
	PidsLimit int64
	// MaxContainers is the advertised ceiling on concurrent sandbox
	// containers (surfaced in CapacityReport so cloudbox can stop
	// routing here when full). Zero means "unset".
	MaxContainers int
	// AllowedImages, when non-empty, is an allowlist of image references
	// (exact match or a "repo/*" wildcard) a create request may use. An
	// empty list allows any image — appropriate for the trusted-fleet
	// Phase-A default; tighten it for multi-tenant Phase B.
	AllowedImages []string
	// ScratchHostPrefix, when non-empty, is the single host path prefix
	// under which bind-mount sources are permitted. Empty (the default)
	// forbids host bind mounts entirely — the safe posture. Anonymous
	// volumes and tmpfs are always allowed; only host-path binds are
	// gated.
	ScratchHostPrefix string
}

// ImageAllowed reports whether ref passes the AllowedImages allowlist. An
// empty allowlist permits everything. A wildcard entry "repo/*" matches
// any ref whose slash-delimited prefix equals "repo/". Matching is on the
// raw reference string the caller supplied (tag/digest included) so an
// operator can pin "docker.io/library/python:3.12" exactly when desired.
func (p Policy) ImageAllowed(ref string) bool {
	if len(p.AllowedImages) == 0 {
		return true
	}
	ref = strings.TrimSpace(ref)
	for _, a := range p.AllowedImages {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.HasSuffix(a, "/*") {
			if strings.HasPrefix(ref, strings.TrimSuffix(a, "*")) {
				return true
			}
			continue
		}
		if ref == a {
			return true
		}
	}
	return false
}
