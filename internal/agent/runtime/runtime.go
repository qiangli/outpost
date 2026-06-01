// Package runtime supervises a podman container that hosts this
// outpost's k3s-agent kubelet. The container's identity is THIS
// outpost's identity (NodeToken, AgentName, overlay credentials);
// from the cluster's POV there's one Node per outpost — the
// container is invisible.
//
// Why a container (not a host subprocess): security isolation
// (kubelet + containerd run under cgroups managed by an outer
// runtime, not directly on the host); cross-platform Linux runtime
// (macOS hosts don't have a host kubelet but can run a privileged
// Linux container via Docker Desktop / Rancher Desktop / ycode-podman /
// Lima). One model, every OS.
//
// Lifecycle: outpost daemon calls Up(ctx, opts). Up locates `podman`
// on PATH, pulls/builds the image if missing, starts a named
// container with the credentials threaded in via env, then streams
// its logs back to outpost's slog. Down(ctx, opts) stops + removes
// the container. Up is idempotent — repeated calls with the same
// AgentName reuse the existing container.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// ErrPodmanNotFound is returned by Up when neither `podman` nor `docker`
// is on PATH. The outpost daemon surfaces it as a clear "install Docker
// Desktop / Rancher Desktop / podman to enable --cluster-mode=agent"
// message; on macOS this is the expected gating.
var ErrPodmanNotFound = errors.New("runtime: no `podman` or `docker` binary on PATH (install Docker Desktop / Rancher Desktop / podman to enable --cluster-mode=agent)")

// Options is the supervisor's input. All fields except ExtraEnv are
// required; LoginServer/AuthKey/PodCIDR may be empty for single-node
// (no-overlay) testing.
type Options struct {
	// AgentName is the outpost's identity. The container's k3s-agent
	// joins as Node <AgentName>; the container itself is named
	// <AgentName>-runtime.
	AgentName string

	// Image is the runtime container image (e.g. "outpost-runtime:dev").
	// Built once via `outpost cluster build-runtime` or pulled from a
	// registry. Empty defaults to DefaultImage.
	Image string

	// NodeToken is the k3s join token cloudbox handed out at pairing
	// (K10<ca-hash>::node:<secret>). Passed into the container via
	// the OUTPOST_NODE_TOKEN env var; never written to disk on the
	// host.
	NodeToken string

	// APIServer is the URL the container's k3s-agent dials. In the
	// cloudbox model this is the loopback STCP visitor inside the
	// container (see overlay package). Empty defaults to
	// "https://127.0.0.1:6443".
	APIServer string

	// CloudboxHost / CloudboxPort are where the container-side frpc
	// dials to establish the matrix-tunnel + STCP visitor. Required
	// for the kubelet-in-container model — entrypoint.sh runs frpc
	// to open 127.0.0.1:APIPort inside the container, tunneling to
	// cloudbox's embedded apiserver. e.g. "ai.dhnt.io" + 443.
	CloudboxHost string
	CloudboxPort int

	// STCPSecret authenticates the STCP visitor on the cloudbox side
	// (cluster.k3s-apiserver publisher). Cluster-wide secret minted
	// at pairing time; passed in via env.
	STCPSecret string

	// MatrixToken is the shared frp auth token (same value cloudbox
	// holds in MATRIX_TOKEN). Empty disables [auth] in frpc.toml.
	MatrixToken string

	// APIPort is the loopback port the STCP visitor binds inside the
	// container (must match cloudbox's ClusterAPIServerPort). Empty
	// defaults to 6443.
	APIPort int

	// KubeletPort is the per-outpost port cloudbox allocated at
	// pairing time (fc.Cluster.KubeletProxyPort). Three things ride
	// on this same number so the apiserver→kubelet hop terminates:
	//   - kubelet binds + advertises this port (so the Node's
	//     daemonEndpoint.Port matches what's reachable);
	//   - the in-container frpc publishes 127.0.0.1:<port> to
	//     cloudbox's loopback at the same port number;
	//   - cloudbox's apiserver dials 127.0.0.1:<port> for this Node.
	// Empty (0) leaves the kubelet on its default 10250 with no
	// outbound publish — `kubectl exec`/`logs`/`port-forward` won't
	// work against this outpost, but the rest of cluster-agent mode
	// keeps functioning. Old pairings without KubeletProxyPort
	// allocated land here.
	KubeletPort int

	// PodCIDR is the per-outpost /24 carved by cloudbox at Exchange
	// time. Empty disables the outpost-cni conflist; k3s falls back
	// to its own defaults (--flannel-backend=none means no pod
	// networking, fine for control-plane-only smoke tests).
	PodCIDR string

	// OverlayLoginServer / OverlayAuthKey turn on tailscaled inside
	// the container. Both must be non-empty; both empty leaves the
	// overlay off (single-node mode).
	OverlayLoginServer string
	OverlayAuthKey     string

	// PodmanBin overrides the autodetected `podman`/`docker` binary.
	// Empty triggers PATH lookup; tests set it.
	PodmanBin string

	// ExtraEnv is appended to the container's env in KEY=VALUE form.
	// Escape hatch for development.
	ExtraEnv []string
}

// DefaultImage is the runtime image tag the outpost daemon expects to
// find. Built by `outpost cluster build-runtime`; can be overridden
// via Options.Image.
const DefaultImage = "outpost-runtime:dev"

// Up ensures the runtime container is running with the supplied
// credentials. Idempotent: if a container with the expected name
// already exists, Up restarts it (so credential changes take effect).
// Returns immediately after the container is started; container exit
// is observed through ctx + a follow-up goroutine the caller spins
// to tail logs.
func Up(ctx context.Context, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return err
	}
	image := opts.Image
	if image == "" {
		image = DefaultImage
	}
	containerName := opts.AgentName + "-runtime"

	// Stop + remove any prior instance so the new env takes effect.
	// Errors swallowed — rm of a non-existent container is normal on
	// first boot.
	_ = exec.CommandContext(ctx, bin, "stop", containerName).Run()
	_ = exec.CommandContext(ctx, bin, "rm", "-f", containerName).Run()

	// Persist only the node-identity directory (/etc/rancher/node) — the
	// agent's `--with-node-id` flag writes node-id + node-password into
	// /etc/rancher/node/node-id and /etc/rancher/node/node-password.k3s.
	// Persisting just that subtree (not the whole /var/lib/rancher/k3s
	// tree) lets the same outpost identity reattach across container
	// restarts without piping containerd's storage layer through a
	// podman named volume (which broke nested FUSE mounts + the
	// kubelet's eviction-manager stats provider).
	nodeIDVol := "outpost-" + opts.AgentName + "-node-id"
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--privileged",
		// Share the VM root's cgroup namespace. The container hosts
		// k3s-agent + kubelet which need cpuset and other v2
		// controllers; a private cgroup namespace doesn't propagate
		// cpuset from the VM root, and the kubelet aborts with
		// "fatal: failed to find cpuset cgroup (v2)". --cgroupns=host
		// is supported by both upstream podman and ycode podman
		// (the latter since the --cgroupns flag was added).
		"--cgroupns=host",
		"-v", nodeIDVol + ":/etc/rancher/node",
		"-e", "OUTPOST_AGENT_NAME=" + opts.AgentName,
		"-e", "OUTPOST_NODE_TOKEN=" + opts.NodeToken,
	}
	if opts.APIServer != "" {
		args = append(args, "-e", "OUTPOST_API_SERVER="+opts.APIServer)
	}
	if opts.CloudboxHost != "" {
		args = append(args, "-e", "OUTPOST_CLOUDBOX_HOST="+opts.CloudboxHost)
	}
	if opts.CloudboxPort != 0 {
		args = append(args, "-e", fmt.Sprintf("OUTPOST_CLOUDBOX_PORT=%d", opts.CloudboxPort))
	}
	if opts.STCPSecret != "" {
		args = append(args, "-e", "OUTPOST_STCP_SECRET="+opts.STCPSecret)
	}
	if opts.MatrixToken != "" {
		args = append(args, "-e", "OUTPOST_MATRIX_TOKEN="+opts.MatrixToken)
	}
	if opts.APIPort != 0 {
		args = append(args, "-e", fmt.Sprintf("OUTPOST_API_PORT=%d", opts.APIPort))
	}
	if opts.KubeletPort != 0 {
		args = append(args, "-e", fmt.Sprintf("OUTPOST_KUBELET_PORT=%d", opts.KubeletPort))
	}
	if opts.PodCIDR != "" {
		args = append(args, "-e", "OUTPOST_POD_CIDR="+opts.PodCIDR)
	}
	if opts.OverlayLoginServer != "" {
		args = append(args, "-e", "OUTPOST_OVERLAY_LOGIN="+opts.OverlayLoginServer)
	}
	if opts.OverlayAuthKey != "" {
		args = append(args, "-e", "OUTPOST_OVERLAY_AUTHKEY="+opts.OverlayAuthKey)
	}
	for _, kv := range opts.ExtraEnv {
		args = append(args, "-e", kv)
	}
	args = append(args, image)

	slog.Info("runtime: starting container",
		"name", containerName, "image", image, "node", opts.AgentName)
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("runtime: %s run: %w (%s)", bin, err, strings.TrimSpace(string(out)))
	}
	slog.Info("runtime: container started", "id", strings.TrimSpace(string(out))[:12])
	return nil
}

// Down stops + removes the container. Used during outpost shutdown +
// when the operator flips --cluster-mode=off.
func Down(ctx context.Context, opts Options) error {
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return err
	}
	containerName := opts.AgentName + "-runtime"
	_ = exec.CommandContext(ctx, bin, "stop", containerName).Run()
	_ = exec.CommandContext(ctx, bin, "rm", "-f", containerName).Run()
	return nil
}

// TailLogs blocks and streams the container's logs to slog at info
// level. Returns when the container exits (or ctx is canceled). The
// caller typically runs this in a goroutine inside the errgroup.
func TailLogs(ctx context.Context, opts Options) error {
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return err
	}
	containerName := opts.AgentName + "-runtime"
	cmd := exec.CommandContext(ctx, bin, "logs", "-f", containerName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("runtime: %s logs: %w", bin, err)
	}
	go streamToSlog(stdout, containerName+".stdout")
	go streamToSlog(stderr, containerName+".stderr")
	return cmd.Wait()
}

// pickPodmanBin returns the first of {opts.PodmanBin, "podman",
// "docker"} found on PATH. Tested-with-ycode podman first (since
// that's our recommended macOS path), regular podman second, docker
// last as a fallback.
func pickPodmanBin(override string) (string, error) {
	if override != "" {
		if _, err := exec.LookPath(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("runtime: PodmanBin %q not found on PATH", override)
	}
	for _, c := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", ErrPodmanNotFound
}

func (o Options) validate() error {
	if o.AgentName == "" {
		return errors.New("runtime: Options.AgentName required")
	}
	if o.NodeToken == "" {
		return errors.New("runtime: Options.NodeToken required")
	}
	return nil
}

func streamToSlog(r interface {
	Read(p []byte) (int, error)
	Close() error
}, source string) {
	defer r.Close()
	buf := make([]byte, 8192)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(carry, buf[:n]...)
			lines := strings.Split(string(chunk), "\n")
			carry = []byte(lines[len(lines)-1])
			for _, line := range lines[:len(lines)-1] {
				if line == "" {
					continue
				}
				slog.Info(source, "line", line)
			}
		}
		if err != nil {
			if len(carry) > 0 {
				slog.Info(source, "line", string(carry))
			}
			return
		}
	}
}

// PodmanAvailable reports whether the runtime is usable on this host.
// Outpost CLI / admincore status surface uses this to render a clear
// "cluster-mode=agent unavailable — install podman" hint instead of
// failing silently at start time.
func PodmanAvailable() bool {
	_, err := pickPodmanBin("")
	return err == nil
}

// _ keeps the time import for future tunables (graceful stop timeout
// is hard-coded above; if we let callers override, time.Duration shows up).
var _ = time.Second
