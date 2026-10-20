package runtime

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedEntrypointBoundsOverlayLoginBeforeK3s(t *testing.T) {
	data, err := fs.ReadFile(imageFS, "image/entrypoint.sh")
	if err != nil {
		t.Fatalf("read embedded entrypoint: %v", err)
	}
	script := string(data)
	timeoutAt := strings.Index(script, `timeout --foreground "${OVERLAY_UP_TIMEOUT}s" tailscale up`)
	if timeoutAt < 0 {
		t.Fatal("boot-time tailscale up is not deadline-bounded")
	}
	k3sAt := strings.LastIndex(script, "exec k3s agent")
	if k3sAt < 0 {
		t.Fatal("entrypoint no longer execs k3s agent")
	}
	if timeoutAt > k3sAt {
		t.Fatal("overlay deadline must guard the pre-k3s startup path")
	}
	if !strings.Contains(script[timeoutAt:k3sAt], "continuing to k3s") {
		t.Fatal("overlay timeout does not explicitly continue to k3s")
	}
}

func TestEmbeddedEntrypointReconcilesIPAMBeforeK3s(t *testing.T) {
	data, err := fs.ReadFile(imageFS, "image/entrypoint.sh")
	if err != nil {
		t.Fatalf("read embedded entrypoint: %v", err)
	}
	script := string(data)
	reconcileAt := strings.Index(script, "/opt/cni/bin/outpost-cni reconcile-ipam")
	if reconcileAt < 0 {
		t.Fatal("entrypoint does not reconcile stale IPAM on runtime recreation")
	}
	k3sAt := strings.LastIndex(script, "exec k3s agent")
	if k3sAt < 0 {
		t.Fatal("entrypoint no longer execs k3s agent")
	}
	if reconcileAt > k3sAt {
		t.Fatal("stale IPAM must be reconciled before k3s starts")
	}
}

func TestEmbeddedEntrypointAPIBindDefaultsToContainerLoopback(t *testing.T) {
	data, err := fs.ReadFile(imageFS, "image/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, `: "${OUTPOST_API_BIND_ADDR:=127.0.0.1}"`) {
		t.Fatal("entrypoint does not default the visitor to container loopback")
	}
	if !strings.Contains(script, `bindAddr = "${OUTPOST_API_BIND_ADDR}"`) {
		t.Fatal("entrypoint does not thread the bridge-only bind override into frpc")
	}
}

func TestEmbeddedServerEntrypointWaitsForKubeconfigBeforeExport(t *testing.T) {
	data, err := fs.ReadFile(imageFS, "image/server-entrypoint.sh")
	if err != nil {
		t.Fatalf("read embedded server entrypoint: %v", err)
	}
	script := string(data)
	waitAt := strings.Index(script, `[ ! -s /etc/rancher/k3s/k3s-internal.yaml ]`)
	if waitAt < 0 {
		t.Fatal("server entrypoint does not wait for k3s to write its kubeconfig")
	}
	sedAt := strings.Index(script, "/etc/rancher/k3s/k3s-internal.yaml > /run/outpost/kubeconfig/k3s.yaml.new")
	if sedAt < 0 {
		t.Fatal("server entrypoint no longer exports the host kubeconfig")
	}
	if waitAt > sedAt {
		t.Fatal("kubeconfig wait must precede the export that reads it")
	}
	if !strings.Contains(script[waitAt:sedAt], `[ -s /etc/rancher/k3s/k3s-internal.yaml ]; then`) {
		t.Fatal("export must be guarded by the kubeconfig existing, not only the port listening")
	}
}

// TestEntrypointFederationVisitor renders the entrypoint's federation block
// under bash: with a federation host + secret it writes frpc-federation.toml
// carrying the karmada-apiserver visitor on 0.0.0.0:15443 and requires port
// 15443 of its supervisor; without them it writes nothing and requires no
// port, so an unfederated runtime is unchanged.
func TestEntrypointFederationVisitor(t *testing.T) {
	data, err := fs.ReadFile(imageFS, "image/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	const begin, end = "# --- federation-visitor (begin) ---", "# --- federation-visitor (end) ---"
	b, e := strings.Index(script, begin), strings.Index(script, end)
	if b < 0 || e < b {
		t.Fatal("entrypoint has no delimited federation-visitor block")
	}
	block := script[b:e]
	bashBin := findSupervisorBash(t)

	render := func(t *testing.T, env ...string) (toml string, ports string) {
		t.Helper()
		tmp := t.TempDir()
		cfg := filepath.Join(tmp, "frpc-federation.toml")
		body := "set -eu\nlog() { :; }\n" + block + "\nprintf '%s' \"${FEDERATION_REQUIRED_PORTS}\"\n"
		cmd := exec.Command(bashBin, "-c", body)
		cmd.Env = append([]string{
			"PATH=" + os.Getenv("PATH"),
			"OUTPOST_AGENT_NAME=member-b",
			"FRPC_FEDERATION_CONFIG=" + cfg,
		}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("render federation block: %v\n%s", err, out)
		}
		if raw, err := os.ReadFile(cfg); err == nil {
			toml = string(raw)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return toml, string(out)
	}

	t.Run("joined", func(t *testing.T) {
		toml, ports := render(t,
			"OUTPOST_FEDERATION_HOST=192.0.2.10",
			"OUTPOST_FEDERATION_TOKEN=tunnel-token",
			"OUTPOST_FEDERATION_STCP_SECRET=stcp-secret",
		)
		for _, want := range []string{
			`serverAddr = "192.0.2.10"`,
			`serverPort = 7000`,
			`user = "member-b"`,
			`loginFailExit = false`,
			`token = "tunnel-token"`,
			`protocol = "tcp"`,
			`name = "karmada-apiserver-visitor"`,
			`type = "stcp"`,
			`serverUser = "control-plane"`,
			`serverName = "karmada-apiserver"`,
			`secretKey = "stcp-secret"`,
			`bindAddr = "0.0.0.0"`,
			`bindPort = 15443`,
		} {
			if !strings.Contains(toml, want) {
				t.Errorf("frpc-federation.toml missing %q:\n%s", want, toml)
			}
		}
		if !containsPort(ports, "15443") {
			t.Errorf("FRPC_REQUIRED_PORTS = %q, want it to include 15443", ports)
		}
	})

	t.Run("not joined", func(t *testing.T) {
		toml, ports := render(t)
		if toml != "" {
			t.Errorf("unfederated runtime wrote frpc-federation.toml:\n%s", toml)
		}
		if containsPort(ports, "15443") {
			t.Errorf("unfederated FRPC_REQUIRED_PORTS = %q includes 15443", ports)
		}
	})

	// The block's outputs must actually drive a third, separate supervisor.
	launch := script[e:]
	for _, want := range []string{
		`FRPC_REQUIRED_PORTS="${FEDERATION_REQUIRED_PORTS}"`,
		`FRPC_CONFIG="${FRPC_FEDERATION_CONFIG}"`,
		`FRPC_LOG=/tmp/frpc-federation.log`,
	} {
		if !strings.Contains(launch, want) {
			t.Errorf("federation supervisor launch missing %q", want)
		}
	}
	if k3sAt := strings.LastIndex(script, "exec k3s agent"); k3sAt < e {
		t.Error("federation visitor must start before k3s agent execs")
	}
}

func containsPort(list, port string) bool {
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) == port {
			return true
		}
	}
	return false
}
