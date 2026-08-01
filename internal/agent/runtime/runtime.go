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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/outpost/internal/agent/nodeaddr"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/binmgr"
)

// ErrPodmanNotFound is returned by Up when neither `podman` nor `docker`
// is on PATH. The outpost daemon surfaces it as a clear "install Docker
// Desktop / Rancher Desktop / podman to enable the agent runtime"
// message; on macOS this is the expected gating.
var ErrPodmanNotFound = errors.New("runtime: no `podman` or `docker` binary on PATH (install Docker Desktop / Rancher Desktop / podman to enable the DKS agent runtime)")

// Options is the supervisor's input. All fields except ExtraEnv are
// required; LoginServer/AuthKey/PodCIDR may be empty for single-node
// (no-overlay) testing.
type Options struct {
	// AgentName is the outpost's identity. The container's k3s-agent
	// joins as Node <AgentName>; the container itself is named
	// <AgentName>-runtime.
	AgentName string
	// HostName is the registered physical host that owns this Node.
	// It may differ from AgentName when the operator overrides the
	// cluster node-name prefix.
	HostName string

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

	// ForceRecreate replaces an otherwise matching runtime. The caller sets
	// this when EnsureImage rebuilt the local image in this process.
	ForceRecreate bool
}

// DefaultImage is the runtime image tag the outpost daemon expects to
// find. Built by `outpost cluster build-runtime`; can be overridden
// via Options.Image.
const DefaultImage = "outpost-runtime:dev"

const (
	nodeIDVolumeSuffix          = "node-id"
	tailscaleStateVolumeSuffix  = "ts-state"
	cniStateVolumeSuffix        = "cni"
	workloadStorageVolumeSuffix = "k3s-storage"
	runtimeConfigLabel          = "io.dhnt.runtime-config-sha256"
)

func runtimeVolumeName(agentName, suffix string) string {
	return "outpost-" + agentName + "-" + suffix
}

// persistentVolumeMounts are deliberately narrow. In particular, never replace
// the k3s-storage mount with /var/lib/rancher/k3s: containerd and its
// snapshotter also live under that parent, and putting the complete tree on an
// outer-runtime volume breaks nested mounts and kubelet filesystem statistics.
//
// The local-path provisioner stores workload PVC contents under
// /var/lib/rancher/k3s/storage. Keeping only that directory on a stable,
// outpost-managed volume lets those PVCs survive a runtime-container recreate
// and a leave/rejoin without coupling the inner container runtime to Podman.
//
// The other mounts retain their existing lifecycle: node-id keeps the kubelet
// identity stable; ts-state keeps the Tailscale machine key stable between
// restarts; and cni keeps the IPAM allocation ledger stable while the same
// inner container runtime survives. On a proven runtime recreation the old
// lease directory is quarantined inside the cni volume before k3s starts.
// Leave purges the latter two membership volumes but not node-id or workload
// storage (see identityVolumes).
func persistentVolumeMounts(agentName string) []string {
	return []string{
		runtimeVolumeName(agentName, nodeIDVolumeSuffix) + ":/etc/rancher/node",
		runtimeVolumeName(agentName, tailscaleStateVolumeSuffix) + ":/var/lib/tailscale",
		runtimeVolumeName(agentName, cniStateVolumeSuffix) + ":/var/lib/cloudbox/cni",
		runtimeVolumeName(agentName, workloadStorageVolumeSuffix) + ":/var/lib/rancher/k3s/storage",
	}
}

// Up ensures the runtime container is running with the supplied
// credentials. Idempotent: if a matching container with the expected name
// already exists, Up reuses it (or starts it if stopped). Configuration/image
// changes replace it and mark the new inner runtime for stale-IPAM recovery.
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

	fingerprint, err := runtimeFingerprint(ctx, bin, image, opts)
	if err != nil {
		return err
	}
	existingFingerprint, running, containerExists := inspectRuntime(ctx, bin, containerName)
	if containerExists && !opts.ForceRecreate && existingFingerprint == fingerprint {
		if running {
			slog.Info("runtime: reusing running container", "name", containerName)
			return nil
		}
		if out, err := exec.CommandContext(ctx, bin, "start", containerName).CombinedOutput(); err != nil {
			return fmt.Errorf("runtime: %s start: %w (%s)", bin, err, strings.TrimSpace(string(out)))
		}
		slog.Info("runtime: restarted existing container", "name", containerName)
		return nil
	}

	// A successful inspect proves that this specific container exists and can
	// safely be removed. An inspect error is deliberately not followed by rm:
	// if the engine itself is unhealthy, the subsequent run fails closed
	// instead of destroying state based on an ambiguous result.
	if containerExists {
		_ = exec.CommandContext(ctx, bin, "stop", containerName).Run()
		if out, err := exec.CommandContext(ctx, bin, "rm", "-f", containerName).CombinedOutput(); err != nil {
			return fmt.Errorf("runtime: %s rm: %w (%s)", bin, err, strings.TrimSpace(string(out)))
		}
	}

	// The CNI volume can outlive a manually removed runtime container. Its
	// presence still proves that this is not a first boot; all leases belong to
	// the erased inner containerd and must be quarantined before new CNI ADDs.
	runtimeRecreated := containerExists || volumeExists(ctx, bin,
		runtimeVolumeName(opts.AgentName, cniStateVolumeSuffix))

	hostName := opts.HostName
	if hostName == "" {
		hostName = opts.AgentName
	}
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--label", runtimeConfigLabel + "=" + fingerprint,
		"--privileged",
		// Share the VM root's cgroup namespace. The container hosts
		// k3s-agent + kubelet which need cpuset and other v2
		// controllers; a private cgroup namespace doesn't propagate
		// cpuset from the VM root, and the kubelet aborts with
		// "fatal: failed to find cpuset cgroup (v2)". --cgroupns=host
		// is supported by both upstream podman and ycode podman
		// (the latter since the --cgroupns flag was added).
		"--cgroupns=host",
	}
	for _, mount := range persistentVolumeMounts(opts.AgentName) {
		args = append(args, "-v", mount)
	}
	args = append(args,
		// Host kernel modules (read-only) so the entrypoint can
		// `modprobe br_netfilter` — required for kube-proxy's ClusterIP
		// DNAT to apply to on-bridge (same-node) service traffic, which
		// is what makes in-cluster DNS work. Without the module the
		// bridge-nf-call sysctls don't even exist.
		"-v", "/lib/modules:/lib/modules:ro",
		"-e", "OUTPOST_AGENT_NAME="+opts.AgentName,
		"-e", "OUTPOST_HOST_NAME="+hostName,
		"-e", "OUTPOST_NODE_TOKEN="+opts.NodeToken,
	)
	if runtimeRecreated {
		args = append(args, "-e", "OUTPOST_RUNTIME_RECREATED=1")
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
		// KubeletPort 0 means no cloudbox allocation — either an old
		// pairing, or a PEER-HOSTED control plane, which has no central
		// party to allocate from. Derive the same number the control
		// plane derives (nodeaddr.KubeletPortForNode) so both ends agree
		// without anything being distributed. The tunnel publishes
		// remotePort == localPort, so this one number has to match on
		// both sides or the apiserver dials a port nothing bound.
		kport := opts.KubeletPort
		if kport == 0 && opts.AgentName != "" {
			kport = nodeaddr.KubeletPortForNode(opts.AgentName)
		}
		args = append(args, "-e", fmt.Sprintf("OUTPOST_KUBELET_PORT=%d", kport))
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
	id := strings.TrimSpace(string(out))
	if len(id) > 12 {
		id = id[:12]
	}
	slog.Info("runtime: container started", "id", id)
	return nil
}

type fingerprintInput struct {
	AgentName     string
	HostName      string
	ImageIdentity string
	NodeToken     string
	APIServer     string
	CloudboxHost  string
	CloudboxPort  int
	STCPSecret    string
	MatrixToken   string
	APIPort       int
	KubeletPort   int
	PodCIDR       string
	OverlayLogin  string
	ExtraEnv      []string
}

func runtimeFingerprint(ctx context.Context, bin, image string, opts Options) (string, error) {
	identity := image
	if out, err := exec.CommandContext(ctx, bin, "image", "inspect", "--format", "{{.Id}}", image).CombinedOutput(); err == nil {
		if inspected := strings.TrimSpace(string(out)); inspected != "" {
			identity = inspected
		}
	}
	input := fingerprintInput{
		AgentName:     opts.AgentName,
		HostName:      opts.HostName,
		ImageIdentity: identity,
		NodeToken:     opts.NodeToken,
		APIServer:     opts.APIServer,
		CloudboxHost:  opts.CloudboxHost,
		CloudboxPort:  opts.CloudboxPort,
		STCPSecret:    opts.STCPSecret,
		MatrixToken:   opts.MatrixToken,
		APIPort:       opts.APIPort,
		KubeletPort:   opts.KubeletPort,
		PodCIDR:       opts.PodCIDR,
		OverlayLogin:  opts.OverlayLoginServer,
		ExtraEnv:      opts.ExtraEnv,
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("runtime: fingerprint config: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func inspectRuntime(ctx context.Context, bin, containerName string) (fingerprint string, running, exists bool) {
	format := fmt.Sprintf(`{{ index .Config.Labels %q }}|{{.State.Running}}`, runtimeConfigLabel)
	out, err := exec.CommandContext(ctx, bin, "inspect", "--format", format, containerName).CombinedOutput()
	if err != nil {
		return "", false, false
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(parts) != 2 {
		return "", false, true
	}
	fingerprint = strings.TrimSpace(parts[0])
	if fingerprint == "<no value>" {
		fingerprint = ""
	}
	return fingerprint, strings.EqualFold(strings.TrimSpace(parts[1]), "true"), true
}

func volumeExists(ctx context.Context, bin, volumeName string) bool {
	return exec.CommandContext(ctx, bin, "volume", "inspect", volumeName).Run() == nil
}

// Down stops + removes the container. Used during outpost shutdown +
// when the operator disables DKS.
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

// identityVolumes are the podman named volumes `leave` purges so a rejoin gets
// a clean OVERLAY identity: the tailscale machine key (ts-state) and the CNI
// IPAM ledger. cloudbox deletes this node's Headscale registration and releases
// its pod CIDR on leave, so re-attaching the old machine key would try to be a
// node the coordinator no longer knows, and the stale IPAM ledger would hand
// out addresses from the previous pod CIDR — both wrong for a fresh rejoin.
//
// The k3s node-id volume is DELIBERATELY EXCLUDED: it carries the k8s node NAME
// + node-password, which are orthogonal to the overlay. Purging it would churn
// the node name on every leave/join (<node>-<hashA> → <node>-<hashB> …),
// orphaning labels/affinity and accreting node objects. Keeping it lets the
// node RETURN under a stable name (the kubelet re-registers it at startup), so
// leave/join is idempotent in the cluster's eyes.
//
// The k3s-storage volume is also DELIBERATELY EXCLUDED. It is workload data,
// not node membership state: cluster leave must not silently delete local-path
// PVC contents.
func identityVolumes(agentName string) []string {
	return []string{
		runtimeVolumeName(agentName, tailscaleStateVolumeSuffix),
		runtimeVolumeName(agentName, cniStateVolumeSuffix),
	}
}

// PurgeVolumes removes a node's persistent-identity volumes so the next join
// mints a FRESH identity (new machine key → clean Headscale registration → the
// overlay can converge). Call ONLY after Down (the container must be gone —
// podman refuses to remove an in-use volume) AND after cloudbox's node teardown
// succeeded. A missing volume is normal (swallowed); `-f` also detaches a
// stopped container's reference.
func PurgeVolumes(ctx context.Context, opts Options) error {
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return err
	}
	for _, v := range identityVolumes(opts.AgentName) {
		_ = exec.CommandContext(ctx, bin, "volume", "rm", "-f", v).Run()
	}
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
	// Our own managed cache first. bashy provisions podman there, so the binary
	// we ship is found regardless of what $PATH the process inherited — a
	// daemon started by launchd/systemd gets a minimal PATH that excludes every
	// package-manager prefix, which previously made this return
	// ErrPodmanNotFound on a host where `which podman` resolved fine.
	//
	// PATH remains a fallback for an operator-installed engine (and for docker,
	// which we do not manage).
	for _, c := range []string{"podman", "docker"} {
		if p := binmgr.CachedBinary(c); p != "" {
			return p, nil
		}
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
// "DKS agent runtime unavailable — install podman" hint instead of
// failing silently at start time.
func PodmanAvailable() bool {
	_, err := pickPodmanBin("")
	return err == nil
}

// _ keeps the time import for future tunables (graceful stop timeout
// is hard-coded above; if we let callers override, time.Duration shows up).
var _ = time.Second

// ExecInContainer runs a command inside the outpost's runtime container
// and returns its combined output.
//
// Used by the overlay refresher to talk to the tailscaled that lives in
// there — checking `tailscale status` and re-running `tailscale up` with a
// fresh key. Going through podman exec (rather than reaching into the
// container's socket) keeps the daemon free of any assumption about the
// container's internals beyond "the tailscale CLI is on its PATH".
//
// A non-nil error covers both "podman could not run it" and "the command
// exited non-zero"; the output is returned either way so callers can log
// what actually happened.
func ExecInContainer(ctx context.Context, opts Options, args ...string) ([]byte, error) {
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return nil, err
	}
	full := append([]string{"exec", opts.AgentName + "-runtime"}, args...)
	out, err := exec.CommandContext(ctx, bin, full...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("runtime: exec %v: %w", args, err)
	}
	return out, nil
}
