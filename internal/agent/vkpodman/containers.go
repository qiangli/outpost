package vkpodman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SpecGenerator is the libpod create-container payload. We mirror only
// the fields the v1.Pod translator emits — adding more is a matter of
// extending the struct on demand. Field names match libpod's JSON
// schema; keep them in sync if you bump podman.
type SpecGenerator struct {
	Name           string            `json:"name,omitempty"`
	Image          string            `json:"image"`
	Command        []string          `json:"command,omitempty"`
	Entrypoint     []string          `json:"entrypoint,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	WorkDir        string            `json:"work_dir,omitempty"`
	Hostname       string            `json:"hostname,omitempty"`
	Terminal       bool              `json:"terminal,omitempty"`
	Stdin          bool              `json:"stdin,omitempty"`
	Remove         bool              `json:"remove,omitempty"`
	RestartPolicy  string            `json:"restart_policy,omitempty"`
	Mounts         []Mount           `json:"mounts,omitempty"`
	Volumes        []NamedVolume     `json:"volumes,omitempty"`
	NetNS          *Namespace        `json:"netns,omitempty"`
	PortMappings   []PortMapping     `json:"portmappings,omitempty"`
	ResourceLimits *ResourceLimits   `json:"resource_limits,omitempty"`
}

// Mount mirrors OCI runtime spec mounts as libpod accepts them.
// Type is "bind" / "tmpfs"; Options is a free-form list passed
// through to runc (e.g. "ro", "rprivate", "noexec").
//
// Named volumes do NOT go here — libpod has a separate `volumes`
// field for those (see NamedVolume). A Mount with Type="volume"
// is silently treated as a bind to a path matching the Source
// string, which yields the misleading "No such device" at start.
type Mount struct {
	Type        string   `json:"Type"`
	Source      string   `json:"Source,omitempty"`
	Destination string   `json:"Destination"`
	Options     []string `json:"Options,omitempty"`
}

// NamedVolume is the libpod-side representation of a Docker-style
// `-v <name>:<dest>:<opts>` named volume reference. Lives on
// SpecGenerator.Volumes (not Mounts). The volume identified by Name
// must exist before container create — pre-create it with
// CreateVolume.
type NamedVolume struct {
	Name    string   `json:"Name"`
	Dest    string   `json:"Dest"`
	Options []string `json:"Options,omitempty"`
}

// Namespace describes one of the OCI namespaces (net/pid/ipc/uts/user).
// NSMode is typically "host", "private", "container:<id>", or "ns:<path>".
// Value is required for the container: / ns: forms; otherwise empty.
type Namespace struct {
	NSMode string `json:"nsmode,omitempty"`
	Value  string `json:"value,omitempty"`
}

// PortMapping is one host-to-container TCP/UDP port forward.
type PortMapping struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      uint16 `json:"host_port,omitempty"`
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol,omitempty"` // "tcp" "udp" "sctp"
}

// ResourceLimits maps a subset of cgroup limits. CPU.Quota / Period
// model "milliCPU" the way Kubernetes does: period=100000, quota=N*100
// for N milliCPU. Memory.Limit is bytes.
type ResourceLimits struct {
	CPU    *CPULimits    `json:"cpu,omitempty"`
	Memory *MemoryLimits `json:"memory,omitempty"`
}

type CPULimits struct {
	Period uint64 `json:"period,omitempty"`
	Quota  int64  `json:"quota,omitempty"`
}

type MemoryLimits struct {
	Limit int64 `json:"limit,omitempty"`
}

// CreateResponse is what /libpod/containers/create returns. Warnings is
// preserved so callers can log them; the container is still usable when
// warnings are present.
type CreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings,omitempty"`
}

// CreateContainer issues POST /libpod/containers/create. Returns the
// new container's ID. The container is created in the "created" state
// and must be started separately.
func (c *Client) CreateContainer(ctx context.Context, spec *SpecGenerator) (*CreateResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, apiPrefix+"/libpod/containers/create", nil, spec)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, statusErr("create container", resp)
	}
	var out CreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("vkpodman: decode create response: %w", err)
	}
	return &out, nil
}

// StartContainer issues POST /libpod/containers/{id}/start. Idempotent —
// starting an already-running container returns 304 (not modified) which
// we treat as success.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, apiPrefix+"/libpod/containers/"+url.PathEscape(id)+"/start", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	default:
		return statusErr("start container", resp)
	}
}

// StopContainer issues POST /libpod/containers/{id}/stop. timeout is
// the seconds-before-SIGKILL grace period; 0 == kill immediately. A
// 304 (already stopped) is treated as success.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{}
	if timeout >= 0 {
		q.Set("timeout", strconv.Itoa(int(timeout/time.Second)))
	}
	resp, err := c.do(ctx, http.MethodPost, apiPrefix+"/libpod/containers/"+url.PathEscape(id)+"/stop", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	default:
		return statusErr("stop container", resp)
	}
}

// RemoveContainer issues DELETE /libpod/containers/{id}. When force is
// true the container is stopped first (libpod handles the sequencing).
// When volumes is true, named anonymous volumes are also removed.
func (c *Client) RemoveContainer(ctx context.Context, id string, force, volumes bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	if volumes {
		q.Set("v", "true")
	}
	resp, err := c.do(ctx, http.MethodDelete, apiPrefix+"/libpod/containers/"+url.PathEscape(id), q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return nil
	default:
		return statusErr("remove container", resp)
	}
}

// InspectContainer issues GET /libpod/containers/{id}/json. Returns
// only the fields the provider's status reporter actually reads — the
// full inspect schema is sprawling and we'd rather track adds explicitly
// than carry every podman field as dead code.
type InspectContainer struct {
	ID        string        `json:"Id"`
	Name      string        `json:"Name"`
	State     InspectState  `json:"State"`
	Config    InspectConfig `json:"Config"`
	Image     string        `json:"Image"`
	ImageName string        `json:"ImageName"`
	Created   time.Time     `json:"Created"`
}

// InspectState mirrors the subset of /containers/{id}/json State we read.
// Status is one of "created" / "running" / "paused" / "exited" /
// "stopped" / "removing" / "stopping" — libpod's terminology, which we
// translate to corev1.ContainerState in the provider.
type InspectState struct {
	Status     string    `json:"Status"`
	Running    bool      `json:"Running"`
	Paused     bool      `json:"Paused"`
	Restarting bool      `json:"Restarting"`
	OOMKilled  bool      `json:"OOMKilled"`
	Dead       bool      `json:"Dead"`
	Pid        int       `json:"Pid"`
	ExitCode   int32     `json:"ExitCode"`
	Error      string    `json:"Error,omitempty"`
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
}

// InspectConfig carries the bits of /Config we use — namely the labels
// we stamp at create time, which is how reconcile distinguishes
// outpost-owned containers from anything else the user runs locally.
type InspectConfig struct {
	Labels map[string]string `json:"Labels"`
	Env    []string          `json:"Env,omitempty"`
}

// InspectContainer fetches the detailed inspect record. Returns a
// wrapped *APIError with Status=404 when the container does not exist,
// so callers can use IsNotFound to distinguish "gone" from "broken".
func (c *Client) InspectContainer(ctx context.Context, id string) (*InspectContainer, error) {
	resp, err := c.do(ctx, http.MethodGet, apiPrefix+"/libpod/containers/"+url.PathEscape(id)+"/json", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("inspect container", resp)
	}
	var out InspectContainer
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("vkpodman: decode inspect: %w", err)
	}
	return &out, nil
}

// ListContainers issues GET /libpod/containers/json. When all is true,
// stopped containers are included as well as running ones. labelFilter,
// when non-nil, restricts the result to containers carrying every given
// key=value label — used by reconcile to enumerate only the containers
// outpost owns (label outpost.io/managed=true).
type ListContainerItem struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`  // "running", "exited", ...
	Status string            `json:"Status"` // human-readable, e.g. "Up 3 minutes"
	Labels map[string]string `json:"Labels"`
}

func (c *Client) ListContainers(ctx context.Context, all bool, labelFilter map[string]string) ([]ListContainerItem, error) {
	q := url.Values{}
	if all {
		q.Set("all", "true")
	}
	if len(labelFilter) > 0 {
		filt := map[string][]string{"label": {}}
		for k, v := range labelFilter {
			if v == "" {
				filt["label"] = append(filt["label"], k)
			} else {
				filt["label"] = append(filt["label"], k+"="+v)
			}
		}
		raw, err := json.Marshal(filt)
		if err != nil {
			return nil, fmt.Errorf("vkpodman: encode list filter: %w", err)
		}
		q.Set("filters", string(raw))
	}
	resp, err := c.do(ctx, http.MethodGet, apiPrefix+"/libpod/containers/json", q, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list containers", resp)
	}
	var out []ListContainerItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("vkpodman: decode list: %w", err)
	}
	return out, nil
}

// PullImage issues POST /libpod/images/pull. The endpoint streams a
// JSON-lines progress body which we discard; the call returns once the
// CreateVolume issues POST /libpod/volumes/create with the given name +
// labels. Returns nil on success and on the "already exists" path —
// the latter is what we hit when a second pod from the same Deployment
// claims a HostPath volume that an earlier pod already created.
//
// Libpod's status code for duplicate-name is inconsistent across
// versions: some return 409 (Conflict), others return 500 with
// "volume already exists" in the body. We accept either, falling back
// to a body-match on 500.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name}
	if len(labels) > 0 {
		body["Labels"] = labels
	}
	resp, err := c.do(ctx, http.MethodPost, apiPrefix+"/libpod/volumes/create", nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusConflict:
		return nil
	case http.StatusInternalServerError:
		// Read once for the message check; if it's not the
		// already-exists case, surface the normal status error using
		// the message we already consumed.
		b, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(b), "already exists") {
			return nil
		}
		return fmt.Errorf("vkpodman: create volume: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	default:
		return statusErr("create volume", resp)
	}
}

// RemoveVolume issues DELETE /libpod/volumes/{name}. force=true asks
// libpod to detach any container still referencing the volume before
// removing it (we set this on DeletePod cleanup so a still-running
// container doesn't block the per-pod volume reap). A missing volume
// returns 404 which we treat as success — idempotent.
func (c *Client) RemoveVolume(ctx context.Context, name string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	resp, err := c.do(ctx, http.MethodDelete, apiPrefix+"/libpod/volumes/"+url.PathEscape(name), q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return statusErr("remove volume", resp)
	}
}

// stream closes (image is fully pulled). reference is the full image
// ref ("docker.io/library/alpine:3.20").
func (c *Client) PullImage(ctx context.Context, reference string) error {
	q := url.Values{"reference": []string{reference}}
	resp, err := c.do(ctx, http.MethodPost, apiPrefix+"/libpod/images/pull", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("pull image", resp)
	}
	// Drain the streaming progress payload so the connection can be
	// reused. Errors mid-stream surface as JSON lines with {"error":..};
	// scan for those rather than swallow silently.
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var line struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&line); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("vkpodman: pull stream: %w", err)
		}
		if line.Error != "" {
			return fmt.Errorf("vkpodman: pull image %q: %s", reference, strings.TrimSpace(line.Error))
		}
	}
	return nil
}
