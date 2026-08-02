package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func validServerOpts(t *testing.T) ServerOptions {
	t.Helper()
	return ServerOptions{
		AgentName:     "host-a",
		TunnelToken:   "tok",
		STCPSecret:    "sec",
		KubeconfigDir: t.TempDir(),
	}
}

// A control plane in front of an apiserver must not start without its
// credentials — an frps with no token accepts every client that can reach it.
func TestServerOptions_ValidateRequiresCredentials(t *testing.T) {
	cases := map[string]func(*ServerOptions){
		"no agent name":    func(o *ServerOptions) { o.AgentName = "" },
		"no tunnel token":  func(o *ServerOptions) { o.TunnelToken = "" },
		"no stcp secret":   func(o *ServerOptions) { o.STCPSecret = "" },
		"no kubeconfigdir": func(o *ServerOptions) { o.KubeconfigDir = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := validServerOpts(t)
			mutate(&o)
			o.applyDefaults()
			if err := o.validate(); err == nil {
				t.Error("accepted an incomplete control-plane config")
			}
		})
	}
	o := validServerOpts(t)
	o.applyDefaults()
	if err := o.validate(); err != nil {
		t.Errorf("rejected a valid config: %v", err)
	}
}

// THE DEFAULT MUST BE LOOPBACK. Enabling a control plane should never
// silently publish an apiserver to whatever network the machine is on.
func TestServerOptions_DefaultsToLoopback(t *testing.T) {
	o := ServerOptions{}
	o.applyDefaults()
	if o.TunnelBindAddr != "127.0.0.1" {
		t.Errorf("bind addr = %q, want loopback", o.TunnelBindAddr)
	}
	if o.TunnelBindPort != 7000 {
		t.Errorf("tunnel port = %d, want 7000", o.TunnelBindPort)
	}
	// THE HOSTED APISERVER MUST NOT DEFAULT TO 6443. A host that JOINS a
	// cluster already binds 127.0.0.1:6443 for that cluster's visitor; a host
	// that also HOSTS one would collide, and kubectl would then reach
	// whichever listener won while presenting the other cluster's CA —
	// surfacing as "certificate signed by unknown authority", which sends you
	// looking at certificates instead of at ports.
	if o.APIPort == 6443 {
		t.Error("hosted apiserver defaulted to 6443, colliding with the join-side visitor")
	}
	if o.APIPort != DefaultControlPlaneAPIPort {
		t.Errorf("api port = %d, want %d", o.APIPort, DefaultControlPlaneAPIPort)
	}
	if o.Image != DefaultImage {
		t.Errorf("image = %q, want the shared runtime image", o.Image)
	}
}

// The control plane and the agent runtime must never collide on a host that
// does both — a name clash would have one silently replace the other.
func TestServerContainerName_DistinctFromAgentRuntime(t *testing.T) {
	if ServerContainerName("host-a") == "host-a-runtime" {
		t.Fatal("control-plane container shares the agent runtime's name")
	}
	if !strings.Contains(ServerContainerName("host-a"), "host-a") {
		t.Error("container name does not identify the host")
	}
}

func TestKubeconfigPath(t *testing.T) {
	dir := "/tmp/cp"
	if got := KubeconfigPath(dir); got != filepath.Join(dir, "k3s.yaml") {
		t.Errorf("path = %q", got)
	}
}

// THE FINGERPRINT IS WHAT DECIDES A RECREATE. Any field that changes what the
// container does must move it — otherwise a reconfigured control plane keeps
// serving the old settings while reporting healthy, which is the failure this
// package is most prone to and the one hardest to notice.
func TestServerFingerprint_ChangesWithEveryMeaningfulField(t *testing.T) {
	ctx := context.Background()
	base := validServerOpts(t)
	base.applyDefaults()
	// A bin that does not exist makes `image inspect` fail, which is fine:
	// the identity falls back to the image name and the hash still covers the
	// options, which is what this test is about.
	const bin = "/nonexistent/podman"

	ref, err := serverFingerprint(ctx, bin, base)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if same, err := serverFingerprint(ctx, bin, base); err != nil || same != ref {
		t.Fatalf("fingerprint is not stable: %v / %s vs %s", err, same, ref)
	}

	mutations := map[string]func(*ServerOptions){
		"tunnel token": func(o *ServerOptions) { o.TunnelToken = "other" },
		"stcp secret":  func(o *ServerOptions) { o.STCPSecret = "other" },
		"bind addr":    func(o *ServerOptions) { o.TunnelBindAddr = "0.0.0.0" },
		"bind port":    func(o *ServerOptions) { o.TunnelBindPort = 7100 },
		"api port":     func(o *ServerOptions) { o.APIPort = 26443 },
		"tls sans":     func(o *ServerOptions) { o.TLSSANs = "10.0.0.5" },
		"cluster cidr": func(o *ServerOptions) { o.ClusterCIDR = "10.42.0.0/16" },
		"service cidr": func(o *ServerOptions) { o.ServiceCIDR = "10.43.0.0/16" },
		"image":        func(o *ServerOptions) { o.Image = "other:tag" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			got, err := serverFingerprint(ctx, bin, o)
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			if got == ref {
				t.Errorf("changing %s did not change the fingerprint — the "+
					"container would keep running the old configuration", name)
			}
		})
	}
}

// Widening the tunnel bind so workers can join directly from the network
// (--bind-addr 0.0.0.0) must publish ONLY the tunnel port that wide — the
// apiserver publish must stay on loopback regardless, since the documented
// contract is that --bind-addr widens the tunnel, never the apiserver.
func TestUpServer_WideTunnelBindDoesNotWidenAPIServerPublish(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("fake Podman executable uses a POSIX shell")
	}
	tempDir := t.TempDir()
	commandLog := filepath.Join(tempDir, "commands.log")
	fakePodman := filepath.Join(tempDir, "podman")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$OUTPOST_TEST_COMMAND_LOG"
if [ "$1" = "inspect" ]; then
  exit 1
fi
if [ "$1" = "run" ]; then
  printf '%s\n' '0123456789abcdef'
fi
`
	if err := os.WriteFile(fakePodman, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_TEST_COMMAND_LOG", commandLog)

	opts := validServerOpts(t)
	opts.PodmanBin = fakePodman
	opts.TunnelBindAddr = "0.0.0.0" // operator widened the tunnel for direct worker joins

	if err := UpServer(context.Background(), opts); err != nil {
		t.Fatalf("UpServer() error = %v", err)
	}

	data, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	var runCmd string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "run ") {
			runCmd = line
		}
	}
	if runCmd == "" {
		t.Fatalf("no `podman run` command was recorded: %s", data)
	}

	wantTunnel := fmt.Sprintf("-p 0.0.0.0:%d:%d", 7000, 7000)
	if !strings.Contains(runCmd, wantTunnel) {
		t.Errorf("run command missing wide tunnel publish %q: %s", wantTunnel, runCmd)
	}

	wantAPI := fmt.Sprintf("-p 127.0.0.1:%d:%d", DefaultControlPlaneAPIPort, controlPlaneContainerAPIPort)
	if !strings.Contains(runCmd, wantAPI) {
		t.Errorf("run command missing loopback apiserver publish %q: %s", wantAPI, runCmd)
	}

	wideAPI := fmt.Sprintf("0.0.0.0:%d", DefaultControlPlaneAPIPort)
	if strings.Contains(runCmd, wideAPI) {
		t.Errorf("apiserver publish followed the wide tunnel bind — security contract violated: %s", runCmd)
	}
}
