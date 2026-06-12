package sandbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Filter is the security core: an http middleware that inspects container
// create / exec-create requests on the way to the local libpod daemon and
// either denies them (escape-bearing knobs) or rewrites them (inject /
// clamp resource caps). Everything else passes through untouched.
//
// It understands two on-the-wire shapes because the daemon serves both:
//   - docker-compat:  POST /v1.x/containers/create   (PascalCase, HostConfig)
//   - libpod (native): POST /v4/libpod/containers/create (lowercase SpecGenerator)
//
// ycode's gateway drives the libpod shape (it uses podman's Go bindings);
// a stock `docker` CLI/SDK drives the docker-compat shape. Both are gated.
//
// Default-deny posture: on a create path whose body can't be parsed, the
// request is rejected rather than forwarded — we never forward a create we
// couldn't vet.
type Filter struct {
	policy Policy
}

// NewFilter returns a Filter bound to policy.
func NewFilter(policy Policy) *Filter { return &Filter{policy: policy} }

// Wrap returns next guarded by the filter. The path it matches on is the
// already-prefix-stripped upstream path (AppRegistry.ProxyTo sets
// r.URL.Path to the post-/app/<name> remainder before the proxy wrap
// runs), so a request to /app/sandbox/v1.41/containers/create arrives here
// as /v1.41/containers/create.
func (f *Filter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isContainerCreatePath(r.URL.Path):
			f.handleMutating(w, r, next, true)
		case isExecCreatePath(r.URL.Path):
			f.handleMutating(w, r, next, false)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// handleMutating reads the JSON body, applies the deny rules, optionally
// injects resource caps (create only — exec has no resource knobs), and
// forwards the rewritten body. isCreate distinguishes container-create
// (full vetting + injection) from exec-create (privileged check only).
func (f *Filter) handleMutating(w http.ResponseWriter, r *http.Request, next http.Handler, isCreate bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20)) // 8 MiB cap on a create body
	_ = r.Body.Close()
	if err != nil {
		deny(w, "reading request body failed")
		return
	}
	// An empty body means "all defaults" — safe; let the daemon handle it.
	if len(bytes.TrimSpace(raw)) == 0 {
		restoreBody(r, raw)
		next.ServeHTTP(w, r)
		return
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		deny(w, "unparseable create body")
		return
	}

	if isCreate {
		if reason := f.denyCreate(m); reason != "" {
			deny(w, reason)
			return
		}
		f.injectLimits(m)
	} else {
		if reason := denyExec(m); reason != "" {
			deny(w, reason)
			return
		}
	}

	rewritten, err := json.Marshal(m)
	if err != nil {
		deny(w, "re-encoding create body failed")
		return
	}
	restoreBody(r, rewritten)
	next.ServeHTTP(w, r)
}

// denyCreate returns a non-empty reason when the create request asks for
// something the sandbox forbids. It dispatches on shape: a PascalCase
// HostConfig (or top-level Image) is docker-compat; otherwise libpod.
func (f *Filter) denyCreate(m map[string]any) string {
	if _, ok := m["HostConfig"]; ok {
		return f.denyDocker(m)
	}
	if _, ok := m["Image"]; ok {
		return f.denyDocker(m)
	}
	return f.denyLibpod(m)
}

// ---- docker-compat shape -------------------------------------------------

func (f *Filter) denyDocker(m map[string]any) string {
	if img := asString(m["Image"]); img != "" && !f.policy.ImageAllowed(img) {
		return "image not allowed: " + img
	}
	hc, _ := asMap(m["HostConfig"])
	if hc == nil {
		return "" // no HostConfig → nothing escape-bearing to set
	}
	if asBool(hc["Privileged"]) {
		return "privileged containers are not permitted"
	}
	if isHostNS(asString(hc["NetworkMode"])) {
		return "host network is not permitted"
	}
	if isHostNS(asString(hc["PidMode"])) {
		return "host PID namespace is not permitted"
	}
	if isHostNS(asString(hc["IpcMode"])) {
		return "host IPC namespace is not permitted"
	}
	if isHostNS(asString(hc["UTSMode"])) {
		return "host UTS namespace is not permitted"
	}
	if isHostNS(asString(hc["UsernsMode"])) {
		return "host user namespace is not permitted"
	}
	if isHostNS(asString(hc["CgroupnsMode"])) {
		return "host cgroup namespace is not permitted"
	}
	if len(asSlice(hc["CapAdd"])) > 0 {
		return "added capabilities are not permitted"
	}
	if len(asSlice(hc["Devices"])) > 0 {
		return "host device passthrough is not permitted"
	}
	if r := f.denyDockerBinds(hc); r != "" {
		return r
	}
	if r := f.denyDockerMounts(hc); r != "" {
		return r
	}
	for _, so := range asSlice(hc["SecurityOpt"]) {
		if unsafeSecurityOpt(asString(so)) {
			return "security-opt that disables confinement is not permitted"
		}
	}
	return ""
}

func (f *Filter) denyDockerBinds(hc map[string]any) string {
	for _, b := range asSlice(hc["Binds"]) {
		// "src:dst[:opts]" — src is a host path when it starts with "/".
		src, _, _ := strings.Cut(asString(b), ":")
		if strings.HasPrefix(src, "/") && !f.hostPathAllowed(src) {
			return "host bind mount is not permitted: " + src
		}
	}
	return ""
}

func (f *Filter) denyDockerMounts(hc map[string]any) string {
	for _, mt := range asSlice(hc["Mounts"]) {
		mm, ok := asMap(mt)
		if !ok {
			continue
		}
		if strings.EqualFold(asString(mm["Type"]), "bind") {
			src := asString(mm["Source"])
			if !f.hostPathAllowed(src) {
				return "host bind mount is not permitted: " + src
			}
		}
	}
	return ""
}

// ---- libpod (SpecGenerator) shape ---------------------------------------

func (f *Filter) denyLibpod(m map[string]any) string {
	// Image reference lives in different fields across podman versions;
	// "image" is the SpecGenerator field, "Image" already handled above.
	if img := asString(m["image"]); img != "" && !f.policy.ImageAllowed(img) {
		return "image not allowed: " + img
	}
	if asBool(m["privileged"]) {
		return "privileged containers are not permitted"
	}
	for field, label := range map[string]string{
		"netns":    "host network",
		"pidns":    "host PID namespace",
		"ipcns":    "host IPC namespace",
		"utsns":    "host UTS namespace",
		"userns":   "host user namespace",
		"cgroupns": "host cgroup namespace",
	} {
		if ns, ok := asMap(m[field]); ok && isHostNS(asString(ns["nsmode"])) {
			return label + " is not permitted"
		}
	}
	if len(asSlice(m["cap_add"])) > 0 {
		return "added capabilities are not permitted"
	}
	if len(asSlice(m["devices"])) > 0 {
		return "host device passthrough is not permitted"
	}
	if len(asSlice(m["device_cgroup_rule"])) > 0 {
		return "device cgroup rules are not permitted"
	}
	for _, mt := range asSlice(m["mounts"]) {
		mm, ok := asMap(mt)
		if !ok {
			continue
		}
		// SpecGenerator mounts mirror the OCI spec: a host bind has
		// type "bind" and a "source" host path.
		if strings.EqualFold(asString(mm["type"]), "bind") {
			src := asString(mm["source"])
			if !f.hostPathAllowed(src) {
				return "host bind mount is not permitted: " + src
			}
		}
	}
	for _, so := range asSlice(m["selinux_opt"]) {
		if strings.Contains(strings.ToLower(asString(so)), "disable") {
			return "selinux label disable is not permitted"
		}
	}
	return ""
}

// denyExec rejects an exec-create that asks for elevated privilege. Both
// shapes name the field the same modulo case (docker "Privileged",
// libpod "privileged"), so check both.
func denyExec(m map[string]any) string {
	if asBool(m["Privileged"]) || asBool(m["privileged"]) {
		return "privileged exec is not permitted"
	}
	return ""
}

// ---- resource-cap injection ---------------------------------------------

// injectLimits sets or clamps the per-container resource caps from policy
// when the request didn't already ask for a tighter value. Operates on
// whichever shape the body is. A zero policy value leaves that knob
// untouched. The goal is DoS containment (memory / pids), not perfect
// scheduling; CPU is injected on the docker shape only (NanoCpus), which
// is where ycode-spawned tools and stock SDKs set it.
func (f *Filter) injectLimits(m map[string]any) {
	if hc, ok := asMap(m["HostConfig"]); ok || mapHasDockerShape(m) {
		if hc == nil {
			hc = map[string]any{}
			m["HostConfig"] = hc
		}
		clampInt(hc, "Memory", f.policy.MaxMemoryBytes)
		clampInt(hc, "NanoCpus", f.policy.NanoCPUs)
		clampInt(hc, "PidsLimit", f.policy.PidsLimit)
		return
	}
	// libpod SpecGenerator: resource_limits.{memory.limit, pids.limit}.
	if f.policy.MaxMemoryBytes <= 0 && f.policy.PidsLimit <= 0 {
		return
	}
	rl, _ := asMap(m["resource_limits"])
	if rl == nil {
		rl = map[string]any{}
		m["resource_limits"] = rl
	}
	if f.policy.MaxMemoryBytes > 0 {
		mem, _ := asMap(rl["memory"])
		if mem == nil {
			mem = map[string]any{}
			rl["memory"] = mem
		}
		clampInt(mem, "limit", f.policy.MaxMemoryBytes)
	}
	if f.policy.PidsLimit > 0 {
		pids, _ := asMap(rl["pids"])
		if pids == nil {
			pids = map[string]any{}
			rl["pids"] = pids
		}
		clampInt(pids, "limit", f.policy.PidsLimit)
	}
}

// ---- helpers ------------------------------------------------------------

// hostPathAllowed reports whether a host bind source is permitted. When no
// scratch prefix is configured (the safe default) host binds are forbidden
// entirely. A non-host path (named volume — doesn't start with "/") is
// always allowed; this function is only consulted for "/"-rooted sources.
func (f *Filter) hostPathAllowed(src string) bool {
	if src == "" || !strings.HasPrefix(src, "/") {
		return true // named volume / tmpfs — not a host path
	}
	prefix := strings.TrimSpace(f.policy.ScratchHostPrefix)
	if prefix == "" {
		return false
	}
	return src == prefix || strings.HasPrefix(src, strings.TrimRight(prefix, "/")+"/")
}

// isContainerCreatePath matches both docker-compat and libpod container
// create endpoints. Suffix match tolerates the version prefix
// (/v1.41/..., /v4.0.0/libpod/...).
func isContainerCreatePath(p string) bool {
	p = strings.ToLower(strings.TrimRight(p, "/"))
	return strings.HasSuffix(p, "/containers/create")
}

// isExecCreatePath matches POST /containers/{id}/exec (exec creation) in
// both shapes. The exec *start* (/exec/{id}/start) carries no privilege
// knob, so only the create is gated.
func isExecCreatePath(p string) bool {
	p = strings.ToLower(strings.TrimRight(p, "/"))
	return strings.Contains(p, "/containers/") && strings.HasSuffix(p, "/exec")
}

// isHostNS reports whether a namespace-mode string requests the host
// namespace ("host" or "host:..."). Case-insensitive.
func isHostNS(mode string) bool {
	mode = strings.ToLower(strings.TrimSpace(mode))
	return mode == "host" || strings.HasPrefix(mode, "host:")
}

// unsafeSecurityOpt reports whether a docker SecurityOpt entry disables a
// confinement layer (seccomp / apparmor / selinux label / masked paths).
func unsafeSecurityOpt(opt string) bool {
	o := strings.ToLower(strings.TrimSpace(opt))
	switch {
	case strings.Contains(o, "seccomp=unconfined"),
		strings.Contains(o, "apparmor=unconfined"),
		strings.Contains(o, "label=disable"),
		strings.Contains(o, "label:disable"),
		strings.Contains(o, "systempaths=unconfined"),
		strings.Contains(o, "unmask"):
		return true
	}
	return false
}

// mapHasDockerShape reports whether a body looks like docker-compat (so
// injectLimits creates a HostConfig). True when the top-level carries a
// PascalCase "Image" — the field docker SDKs always set.
func mapHasDockerShape(m map[string]any) bool {
	_, ok := m["Image"]
	return ok
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// clampInt sets m[key] to limit when the existing value is absent, zero,
// or larger than limit (i.e. the caller asked for more than allowed).
// JSON numbers decode to float64. A non-positive limit is a no-op.
func clampInt(m map[string]any, key string, limit int64) {
	if limit <= 0 {
		return
	}
	cur, ok := m[key].(float64)
	if !ok || cur <= 0 || int64(cur) > limit {
		m[key] = limit
	}
}

// restoreBody replaces r.Body with a reader over b and fixes the
// Content-Length bookkeeping so the reverse proxy forwards the (possibly
// rewritten) body correctly.
func restoreBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", strconv.Itoa(len(b)))
}

// deny writes a docker-shaped error response (the "message" envelope the
// docker/podman clients understand) with a 403.
func deny(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "sandbox: " + reason})
}
