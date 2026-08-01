package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Self-hosted control plane: `k3s server` + `frps` in ONE container.
//
// This is the server-side twin of Up (which runs `k3s agent`), and it reuses
// this package's supervision machinery rather than duplicating it — same
// podman detection, same fingerprint-and-recreate, same log streaming, same
// image.
//
// WHY BOTH PROCESSES ARE IN THE CONTAINER, and why frps did NOT stay in the
// outpost process where it was first built: the apiserver reaches a worker's
// kubelet through a port frps publishes on loopback, and loopback is
// per-network-namespace. Splitting them put the two halves in different
// namespaces — on macOS, literally different machines, since podman runs a
// VM — so the node-address reconciler's work was correct and unreachable, and
// `kubectl logs` against any tunnelled node failed with a 502 while
// scheduling and pod execution worked fine. Cloudbox has always run its
// apiserver and its frps in one process; this is the same arrangement.

// ServerOptions configures the control-plane container.
type ServerOptions struct {
	// AgentName identifies the host; the container is <AgentName>-control-plane.
	AgentName string
	// Image defaults to DefaultImage — the SAME image the agent runtime uses,
	// so an operator never has to keep two images in step.
	Image string

	// TunnelToken gates worker frpc logins.
	TunnelToken string
	// STCPSecret authorizes visitors of the published apiserver proxy.
	STCPSecret string

	// TunnelBindAddr / TunnelBindPort are where the container PUBLISHES its
	// frps to the host. Defaults 127.0.0.1:7000 — loopback, so enabling a
	// control plane never silently exposes it to the network.
	TunnelBindAddr string
	TunnelBindPort int

	// APIPort is the apiserver's port, used BOTH inside the container and as
	// the published host port so the kubeconfig k3s writes is valid on the
	// host unmodified.
	//
	// DEFAULT 16443, NOT 6443, and the difference is load-bearing. A host that
	// JOINS a cluster already binds 127.0.0.1:6443 — that is where its STCP
	// visitor puts the apiserver it joins. A host that also HOSTS a plane
	// would collide, and the collision is silent and vicious: kubectl reaches
	// whichever listener won the port while presenting the OTHER cluster's CA,
	// so it reports `certificate signed by unknown authority` and every
	// instinct says "bad certificate" rather than "wrong server".
	//
	// Hosting and joining are independent decisions, so they must not contend
	// for one port.
	APIPort int

	// KubeconfigDir is a HOST directory the container writes its admin
	// kubeconfig into. This is what makes the cluster operable without
	// `podman exec`, and what cluster.control_plane_kubeconfig points at.
	KubeconfigDir string

	// TLSSANs are extra apiserver certificate SANs, comma-separated. Needed
	// when workers reach this plane by an address other than loopback.
	TLSSANs string

	ClusterCIDR string
	ServiceCIDR string

	PodmanBin     string
	ForceRecreate bool
}

// DefaultControlPlaneAPIPort is where a HOSTED apiserver listens. Deliberately
// not 6443: that belongs to the visitor for a cluster this host JOINS.
const DefaultControlPlaneAPIPort = 16443

// ServerContainerName is the container this host's control plane runs in.
func ServerContainerName(agentName string) string { return agentName + "-control-plane" }

// KubeconfigPath is where the admin kubeconfig lands on the HOST.
func KubeconfigPath(dir string) string { return filepath.Join(dir, "k3s.yaml") }

func (o ServerOptions) validate() error {
	if strings.TrimSpace(o.AgentName) == "" {
		return fmt.Errorf("runtime: control plane requires AgentName")
	}
	if strings.TrimSpace(o.TunnelToken) == "" {
		return fmt.Errorf("runtime: control plane requires TunnelToken")
	}
	if strings.TrimSpace(o.STCPSecret) == "" {
		return fmt.Errorf("runtime: control plane requires STCPSecret")
	}
	if strings.TrimSpace(o.KubeconfigDir) == "" {
		return fmt.Errorf("runtime: control plane requires KubeconfigDir")
	}
	return nil
}

func (o *ServerOptions) applyDefaults() {
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.TunnelBindAddr == "" {
		o.TunnelBindAddr = "127.0.0.1"
	}
	if o.TunnelBindPort == 0 {
		o.TunnelBindPort = 7000
	}
	if o.APIPort == 0 {
		o.APIPort = DefaultControlPlaneAPIPort
	}
}

// serverFingerprint decides whether a running container still matches the
// desired config. Every field that changes what the container DOES belongs
// here — a missing one leaves a control plane serving the previous settings
// while reporting healthy, which is the failure mode this whole package is
// most prone to.
func serverFingerprint(ctx context.Context, bin string, o ServerOptions) (string, error) {
	identity := o.Image
	if out, err := exec.CommandContext(ctx, bin, "image", "inspect", "--format", "{{.Id}}", o.Image).CombinedOutput(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			identity = s
		}
	}
	b, err := json.Marshal(struct {
		ServerOptions
		ImageIdentity string
	}{ServerOptions: o, ImageIdentity: identity})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// UpServer brings up (or reattaches to) this host's control plane. Idempotent.
func UpServer(ctx context.Context, opts ServerOptions) error {
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return err
	}
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.KubeconfigDir, 0o700); err != nil {
		return fmt.Errorf("runtime: control plane kubeconfig dir: %w", err)
	}

	name := ServerContainerName(opts.AgentName)
	fingerprint, err := serverFingerprint(ctx, bin, opts)
	if err != nil {
		return err
	}
	existing, running, exists := inspectRuntime(ctx, bin, name)
	if exists && !opts.ForceRecreate && existing == fingerprint {
		if running {
			slog.Info("control plane: reusing running container", "name", name)
			return nil
		}
		if out, err := exec.CommandContext(ctx, bin, "start", name).CombinedOutput(); err != nil {
			return fmt.Errorf("runtime: %s start: %w (%s)", bin, err, strings.TrimSpace(string(out)))
		}
		slog.Info("control plane: restarted existing container", "name", name)
		return nil
	}
	if exists {
		_ = exec.CommandContext(ctx, bin, "stop", name).Run()
		if out, err := exec.CommandContext(ctx, bin, "rm", "-f", name).CombinedOutput(); err != nil {
			return fmt.Errorf("runtime: %s rm: %w (%s)", bin, err, strings.TrimSpace(string(out)))
		}
	}

	args := []string{
		"run", "-d",
		"--name", name,
		"--label", runtimeConfigLabel + "=" + fingerprint,
		"--privileged",
		"--cgroupns=host",
		// The one entrypoint difference between the two roles.
		"--entrypoint", "/server-entrypoint.sh",
		// Cluster state must survive a recreate, or every restart mints a new
		// cluster and every worker's join token becomes invalid.
		"-v", serverDataVolume(opts.AgentName) + ":/var/lib/rancher/k3s",
		// The admin kubeconfig is written HERE so the host can read it without
		// `podman exec` — that path is what makes the cluster operable.
		"-v", opts.KubeconfigDir + ":/etc/rancher/k3s",
		// Workers reach frps through this — honors the operator's
		// TunnelBindAddr, so `--bind-addr 0.0.0.0` lets workers join
		// directly from the network.
		"-p", fmt.Sprintf("%s:%d:%d", opts.TunnelBindAddr, opts.TunnelBindPort, opts.TunnelBindPort),
		// Host-side kubectl reaches the apiserver through this loopback
		// publish directly; workers reach it only through the STCP visitor
		// over frps. This publish must never follow TunnelBindAddr, or
		// widening the tunnel bind for worker joins also exposes the
		// apiserver to the network, which contradicts the documented
		// contract of --bind-addr.
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", opts.APIPort, opts.APIPort),
		"-e", "OUTPOST_TUNNEL_TOKEN=" + opts.TunnelToken,
		"-e", "OUTPOST_STCP_SECRET=" + opts.STCPSecret,
		"-e", fmt.Sprintf("OUTPOST_TUNNEL_PORT=%d", opts.TunnelBindPort),
		"-e", fmt.Sprintf("OUTPOST_API_PORT=%d", opts.APIPort),
	}
	if opts.TLSSANs != "" {
		args = append(args, "-e", "OUTPOST_TLS_SAN="+opts.TLSSANs)
	}
	if opts.ClusterCIDR != "" {
		args = append(args, "-e", "OUTPOST_CLUSTER_CIDR="+opts.ClusterCIDR)
	}
	if opts.ServiceCIDR != "" {
		args = append(args, "-e", "OUTPOST_SERVICE_CIDR="+opts.ServiceCIDR)
	}
	args = append(args, opts.Image)

	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("runtime: %s run (control plane): %w (%s)", bin, err, strings.TrimSpace(string(out)))
	}
	slog.Info("control plane: container started",
		"name", name, "tunnel", fmt.Sprintf("%s:%d", opts.TunnelBindAddr, opts.TunnelBindPort),
		"kubeconfig", KubeconfigPath(opts.KubeconfigDir))
	return nil
}

// DownServer stops and removes the control-plane container. The data volume
// is deliberately KEPT: removing it destroys the cluster's identity, and a
// stop is not a request to do that.
func DownServer(ctx context.Context, opts ServerOptions) error {
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return nil // nothing to stop if there is no engine
	}
	name := ServerContainerName(opts.AgentName)
	_ = exec.CommandContext(ctx, bin, "stop", name).Run()
	_ = exec.CommandContext(ctx, bin, "rm", "-f", name).Run()
	return nil
}

func serverDataVolume(agentName string) string {
	return "outpost-" + agentName + "-control-plane-data"
}
