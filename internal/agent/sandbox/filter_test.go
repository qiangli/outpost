package sandbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runFilter sends a POST with the given path + JSON body through the
// filter and returns the recorder plus the body the upstream actually
// received (empty when the filter denied before forwarding).
func runFilter(t *testing.T, p Policy, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var forwarded map[string]any
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &forwarded)
		}
		w.WriteHeader(http.StatusCreated)
	})
	h := NewFilter(p).Wrap(upstream)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rr, forwarded
}

func TestFilter_DockerDenials(t *testing.T) {
	const path = "/v1.41/containers/create"
	cases := []struct {
		name string
		body string
	}{
		{"privileged", `{"Image":"alpine","HostConfig":{"Privileged":true}}`},
		{"host-network", `{"Image":"alpine","HostConfig":{"NetworkMode":"host"}}`},
		{"host-pid", `{"Image":"alpine","HostConfig":{"PidMode":"host"}}`},
		{"host-ipc", `{"Image":"alpine","HostConfig":{"IpcMode":"host"}}`},
		{"host-uts", `{"Image":"alpine","HostConfig":{"UTSMode":"host"}}`},
		{"userns-host", `{"Image":"alpine","HostConfig":{"UsernsMode":"host"}}`},
		{"cap-add", `{"Image":"alpine","HostConfig":{"CapAdd":["SYS_ADMIN"]}}`},
		{"devices", `{"Image":"alpine","HostConfig":{"Devices":[{"PathOnHost":"/dev/sda"}]}}`},
		{"host-bind", `{"Image":"alpine","HostConfig":{"Binds":["/etc:/host-etc"]}}`},
		{"host-mount", `{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"bind","Source":"/root","Target":"/r"}]}}`},
		{"seccomp-unconfined", `{"Image":"alpine","HostConfig":{"SecurityOpt":["seccomp=unconfined"]}}`},
		{"apparmor-unconfined", `{"Image":"alpine","HostConfig":{"SecurityOpt":["apparmor=unconfined"]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, fwd := runFilter(t, Policy{}, path, tc.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", rr.Code)
			}
			if fwd != nil {
				t.Fatalf("denied request must not reach upstream; got %v", fwd)
			}
			if !strings.Contains(rr.Body.String(), "sandbox:") {
				t.Errorf("error body should be sandbox-shaped, got %q", rr.Body.String())
			}
		})
	}
}

func TestFilter_LibpodDenials(t *testing.T) {
	const path = "/v4.0.0/libpod/containers/create"
	cases := []struct {
		name string
		body string
	}{
		{"privileged", `{"image":"alpine","privileged":true}`},
		{"host-netns", `{"image":"alpine","netns":{"nsmode":"host"}}`},
		{"host-pidns", `{"image":"alpine","pidns":{"nsmode":"host"}}`},
		{"cap-add", `{"image":"alpine","cap_add":["SYS_ADMIN"]}`},
		{"devices", `{"image":"alpine","devices":[{"path":"/dev/sda"}]}`},
		{"host-bind", `{"image":"alpine","mounts":[{"type":"bind","source":"/etc","destination":"/e"}]}`},
		{"selinux-disable", `{"image":"alpine","selinux_opt":["disable"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, fwd := runFilter(t, Policy{}, path, tc.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", rr.Code)
			}
			if fwd != nil {
				t.Fatalf("denied request must not reach upstream; got %v", fwd)
			}
		})
	}
}

func TestFilter_AllowsNormalCreate_AndInjectsDockerLimits(t *testing.T) {
	p := Policy{MaxMemoryBytes: 512 << 20, NanoCPUs: 1e9, PidsLimit: 256}
	rr, fwd := runFilter(t, p, "/v1.41/containers/create",
		`{"Image":"python:3.12","Cmd":["python","-c","print(1)"]}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	hc, ok := fwd["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("HostConfig should have been injected; body=%v", fwd)
	}
	if hc["Memory"].(float64) != float64(512<<20) {
		t.Errorf("Memory=%v, want %d", hc["Memory"], 512<<20)
	}
	if hc["NanoCpus"].(float64) != 1e9 {
		t.Errorf("NanoCpus=%v, want 1e9", hc["NanoCpus"])
	}
	if hc["PidsLimit"].(float64) != 256 {
		t.Errorf("PidsLimit=%v, want 256", hc["PidsLimit"])
	}
}

func TestFilter_ClampsOverLargeRequest(t *testing.T) {
	p := Policy{MaxMemoryBytes: 256 << 20}
	_, fwd := runFilter(t, p, "/v1.41/containers/create",
		`{"Image":"alpine","HostConfig":{"Memory":1073741824}}`)
	hc := fwd["HostConfig"].(map[string]any)
	if hc["Memory"].(float64) != float64(256<<20) {
		t.Errorf("Memory=%v, want clamp to %d", hc["Memory"], 256<<20)
	}
}

func TestFilter_LibpodInjectsResourceLimits(t *testing.T) {
	p := Policy{MaxMemoryBytes: 128 << 20, PidsLimit: 64}
	_, fwd := runFilter(t, p, "/v4.0.0/libpod/containers/create", `{"image":"alpine"}`)
	rl, ok := fwd["resource_limits"].(map[string]any)
	if !ok {
		t.Fatalf("resource_limits should have been injected; body=%v", fwd)
	}
	mem := rl["memory"].(map[string]any)
	if mem["limit"].(float64) != float64(128<<20) {
		t.Errorf("memory.limit=%v, want %d", mem["limit"], 128<<20)
	}
	pids := rl["pids"].(map[string]any)
	if pids["limit"].(float64) != 64 {
		t.Errorf("pids.limit=%v, want 64", pids["limit"])
	}
}

func TestFilter_ScratchBindAllowed(t *testing.T) {
	p := Policy{ScratchHostPrefix: "/var/lib/outpost/sandbox"}
	rr, _ := runFilter(t, p, "/v1.41/containers/create",
		`{"Image":"alpine","HostConfig":{"Binds":["/var/lib/outpost/sandbox/work:/work"]}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("scratch-dir bind should be allowed; status=%d (%s)", rr.Code, rr.Body.String())
	}
	// A sibling path that merely shares a prefix string must NOT slip through.
	rr2, _ := runFilter(t, p, "/v1.41/containers/create",
		`{"Image":"alpine","HostConfig":{"Binds":["/var/lib/outpost/sandbox-evil:/x"]}}`)
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("prefix-sibling bind must be denied; status=%d", rr2.Code)
	}
}

func TestFilter_NamedVolumeAllowed(t *testing.T) {
	rr, _ := runFilter(t, Policy{}, "/v1.41/containers/create",
		`{"Image":"alpine","HostConfig":{"Binds":["myvol:/data"]}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("named volume must be allowed; status=%d", rr.Code)
	}
}

func TestFilter_ImageAllowlist(t *testing.T) {
	p := Policy{AllowedImages: []string{"docker.io/library/python:3.12", "ghcr.io/acme/*"}}
	ok, _ := runFilter(t, p, "/v1.41/containers/create", `{"Image":"docker.io/library/python:3.12"}`)
	if ok.Code != http.StatusCreated {
		t.Errorf("allowlisted exact image denied; status=%d", ok.Code)
	}
	wild, _ := runFilter(t, p, "/v4.0.0/libpod/containers/create", `{"image":"ghcr.io/acme/runner:v1"}`)
	if wild.Code != http.StatusCreated {
		t.Errorf("allowlisted wildcard image denied; status=%d", wild.Code)
	}
	bad, _ := runFilter(t, p, "/v1.41/containers/create", `{"Image":"evil/miner:latest"}`)
	if bad.Code != http.StatusForbidden {
		t.Errorf("non-allowlisted image should be denied; status=%d", bad.Code)
	}
}

func TestFilter_ExecPrivilegedDenied(t *testing.T) {
	rr, _ := runFilter(t, Policy{}, "/v1.41/containers/abc123/exec", `{"Privileged":true,"Cmd":["sh"]}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("privileged exec must be denied; status=%d", rr.Code)
	}
	ok, _ := runFilter(t, Policy{}, "/v1.41/containers/abc123/exec", `{"Cmd":["sh"]}`)
	if ok.Code != http.StatusCreated {
		t.Fatalf("normal exec should pass; status=%d", ok.Code)
	}
}

func TestFilter_NonCreatePathsPassThrough(t *testing.T) {
	// A privileged-looking body on a non-create path must NOT be inspected
	// (e.g. GET /containers/json listing) — the filter only guards creates.
	rr, _ := runFilter(t, Policy{}, "/v1.41/containers/json", `{"Privileged":true}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("non-create path should pass through; status=%d", rr.Code)
	}
}

func TestFilter_UnparseableCreateDenied(t *testing.T) {
	rr, fwd := runFilter(t, Policy{}, "/v1.41/containers/create", `{not json`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unparseable create body must be denied; status=%d", rr.Code)
	}
	if fwd != nil {
		t.Fatal("unparseable create must not reach upstream")
	}
}
