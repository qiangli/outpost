//go:build !windows

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The peer-hosted DKS runbook (docs/cluster-peer.md, "Host a control plane")
// makes two promises about the listeners a hosted plane opens:
//
//   - "The hosted apiserver is published on `127.0.0.1:16443`." — stated
//     unconditionally, directly after the --bind-addr paragraph.
//   - "--bind-addr 0.0.0.0 ... expose that listener only on a network you
//     intend workers to use" — "that listener" is the TUNNEL listener;
//     nothing says the apiserver moves with it.
//
// The code contradicts both: UpServer publishes the apiserver port with the
// SAME host address as the tunnel port —
//
//	"-p", fmt.Sprintf("%s:%d:%d", opts.TunnelBindAddr, opts.APIPort, opts.APIPort)
//
// so `outpost cluster control-plane on --bind-addr 0.0.0.0` silently exposes
// the k8s apiserver on every interface. Workers never need that: they reach
// the apiserver through the STCP visitor over frps, and conf.TunnelSANs()
// itself refuses to mint a SAN for 0.0.0.0 ("names no reachable address") —
// so this is network exposure of the control plane with no functioning
// certificate path, undisclosed by the runbook that tells operators the
// widened bind affects only the tunnel.
//
// This test asserts the contract the runbook sells: widening the tunnel bind
// must leave the apiserver publish on loopback. It FAILS on the current code.
// Resolving it means either fixing UpServer to pin the apiserver publish to
// 127.0.0.1 (matching the doc and the repo's loopback-is-load-bearing
// convention) or amending docs/cluster-peer.md + embedded_docs to disclose
// that --bind-addr also exposes 16443 — not merging the two as-is.
func TestUpServer_WideTunnelBindKeepsAPIServerLoopback(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")

	// Stub podman: no container exists, image inspect succeeds, `run`
	// records its argv and reports a container id.
	stub := filepath.Join(dir, "podman-stub")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + log + `
case "$1" in
  image) echo sha256:stub ;;
  inspect) exit 1 ;;
  run) echo cid-stub ;;
esac
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := ServerOptions{
		AgentName:      "host-a",
		TunnelToken:    "tok",
		STCPSecret:     "sec",
		KubeconfigDir:  filepath.Join(dir, "kube"),
		TunnelBindAddr: "0.0.0.0", // the documented "accept joins from the network" knob
		PodmanBin:      stub,
	}
	if err := UpServer(context.Background(), opts); err != nil {
		t.Fatalf("UpServer with stub engine: %v", err)
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("stub recorded no calls: %v", err)
	}
	var runLine string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "run ") {
			runLine = line
			break
		}
	}
	if runLine == "" {
		t.Fatalf("no `run` invocation recorded; calls were:\n%s", b)
	}

	if !strings.Contains(runLine, "-v "+opts.KubeconfigDir+":/run/outpost/kubeconfig") {
		t.Fatalf("missing dedicated kubeconfig mount: %s", runLine)
	}
	if strings.Contains(runLine, ":/etc/rancher/k3s") {
		t.Fatalf("k3s private directory must not be mounted: %s", runLine)
	}

	// The tunnel listener is the one the operator chose to widen.
	if !strings.Contains(runLine, "0.0.0.0:7000:7000") {
		t.Errorf("tunnel publish should honor --bind-addr: %s", runLine)
	}
	// The apiserver must stay where the runbook says it is: loopback.
	if !strings.Contains(runLine, "127.0.0.1:16443:6443") {
		t.Errorf("apiserver publish is not loopback — docs/cluster-peer.md promises \"The hosted apiserver is published on 127.0.0.1:16443\", got:\n%s", runLine)
	}
	if strings.Contains(runLine, "0.0.0.0:16443:16443") {
		t.Errorf("apiserver published on all interfaces — --bind-addr 0.0.0.0 is documented as widening only the tunnel listener, got:\n%s", runLine)
	}
}

func TestServerEntrypointDoesNotAdvertiseLoopback(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("image", "server-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") &&
			strings.Contains(line, "--advertise-address=127.0.0.1") {
			t.Fatal("kubernetes rejects a loopback advertise address for the default Service endpoint")
		}
	}
	if !strings.Contains(script, "--tls-san=127.0.0.1") {
		t.Fatal("the host-published loopback apiserver still needs a loopback TLS SAN")
	}
	if !strings.Contains(script, `--advertise-address="$ADVERTISE_ADDR"`) {
		t.Fatal("k3s must advertise its non-loopback container address")
	}
}

func TestPeerMetricsServerIsColocatedWithoutEnablingAgent(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("image", "server-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, required := range []string{
		"--disable-agent",
		"--disable-cloud-controller",
		"--disable=coredns,servicelb,traefik,local-storage,metrics-server",
		"--egress-selector-mode=disabled",
		"--kube-apiserver-arg=kubelet-preferred-address-types=ExternalIP",
		"--kube-apiserver-arg=kubelet-certificate-authority=",
		"OUTPOST_METRICS_ADDRESS=\"${ADVERTISE_ADDR}\"",
		"/usr/local/bin/metrics-server-supervisor.sh",
		`kill -0 "${METRICS_PID:-0}"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("server entrypoint missing colocated metrics contract %q", required)
		}
	}
	if !strings.Contains(script, "not k3s's embedded cloud controller, owns") {
		t.Error("server entrypoint does not document peer ownership of the reconciled ExternalIP")
	}
	if strings.Contains(script, "--egress-selector-mode=pod") ||
		strings.Contains(script, "--egress-selector-mode=cluster") ||
		strings.Contains(script, "--egress-selector-mode=agent") {
		t.Error("peer apiserver must dial colocated FRP and metrics endpoints directly")
	}
	if !strings.Contains(script, "token-authenticated FRP proxy") {
		t.Error("kubelet CA relaxation must document the authenticated tunnel boundary")
	}
	if strings.Contains(script, "hostNetwork:true") || strings.Contains(script, "patch deployment metrics-server") {
		t.Error("peer metrics must not resurrect the worker-hostNetwork patch disproved by the live gate")
	}
	if strings.Contains(script, "kubelet-preferred-address-types=ExternalIP,") {
		t.Error("apiserver must not fall back from reconciler-owned ExternalIP to unreachable worker addresses")
	}

}

func TestPeerMetricsResourcesPreserveTunnelBoundary(t *testing.T) {
	helper := filepath.Join("image", "metrics-server-supervisor.sh")
	cmd := exec.Command("/bin/sh", "-c",
		`OUTPOST_METRICS_LIB_ONLY=1 . "$1"; render_metrics_resources 10.88.0.7`, "sh", helper)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render metrics resources: %v\n%s", err, out)
	}
	yaml := string(out)
	for _, required := range []string{
		`ip: "10.88.0.7"`,
		"addressType: IPv4",
		"kubernetes.io/service-name: metrics-server",
		"endpointslice.kubernetes.io/skip-mirror",
		"name: v1beta1.metrics.k8s.io",
		"name: system:metrics-server",
		"name: extension-apiserver-authentication-reader",
	} {
		if !strings.Contains(yaml, required) {
			t.Errorf("rendered metrics resources missing %q", required)
		}
	}
	for _, forbidden := range []string{"kind: Deployment", "hostNetwork:", "NodePort", "LoadBalancer"} {
		if strings.Contains(yaml, forbidden) {
			t.Errorf("rendered metrics resources cross the colocated-process boundary with %q", forbidden)
		}
	}
	if strings.Contains(yaml, "clusterIP: None") {
		t.Error("aggregation layer cannot route an APIService through a headless Service")
	}

	helperBody, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	s := string(helperBody)
	for _, required := range []string{
		"outpost metrics-exec",
		`--kubeconfig="$internal_kubeconfig"`,
		"--node-selector='outpost.dhnt.io/runtime!=virtual'",
		"--kubelet-preferred-address-types=ExternalIP",
		"--kubelet-use-node-status-port", "--kubelet-insecure-tls",
		`outpost certgen metrics --key "$work/client.key" --cert "$work/client.crt"`,
		`--ca "$client_ca" --ca-key "$client_key"`,
		`rotate_ticks="${OUTPOST_METRICS_ROTATE_TICKS:-8640}"`,
		`-lt "$rotate_ticks"`,
		`--as=system:serviceaccount:kube-system:metrics-server`,
		`outpost netaddr add "$service_ip/32" dev lo`,
		`outpost tlsproxy --listen "${service_ip}:443"`,
		`migrating headless metrics Service`,
	} {
		if !strings.Contains(s, required) {
			t.Errorf("metrics supervisor missing security/routing contract %q", required)
		}
	}
	for _, forbidden := range []string{"system:masters", "server: https://127.0.0.1:16443"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("metrics supervisor contains over-privileged or host-routed credential %q", forbidden)
		}
	}
	if strings.Contains(s, "kubelet-preferred-address-types=ExternalIP,") {
		t.Error("metrics-server must fail closed rather than fall back to an unreachable/non-tunnel node address")
	}
	if strings.Contains(s, "--node-selector='outpost.dhnt.io/runtime=agent'") {
		t.Error("metrics-server selector must retain unlabeled legacy real agents; use runtime!=virtual")
	}
}

func TestControlPlaneReportsFailedChild(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("image", "server-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, supervisor, ok := strings.Cut(string(b), "# Exit if EITHER half dies.")
	if !ok {
		t.Fatal("missing supervisor")
	}
	// Exercise real child exit statuses, including an unexpected clean exit.
	for _, code := range []string{"0", "7"} {
		t.Run(code, func(t *testing.T) {
			script := `set -eu
log() { echo "$*"; }
sleep 30 &
FRPS_PID=$!
sleep 30 &
K3S_PID=$!
(exit ` + code + `) &
METRICS_PID=$!
# Let the short-lived child exit before entering the supervisor.
sleep 0.1
` + "# Exit if EITHER half dies." + supervisor
			out, err := exec.Command("bash", "-c", script).CombinedOutput()
			if err == nil {
				t.Fatal("supervisor survived a failed child")
			}
			if !strings.Contains(string(out), "metrics-supervisor (pid ") ||
				!strings.Contains(string(out), "exited with code "+code) {
				t.Fatalf("missing child diagnostic: %s", out)
			}
		})
	}
}

// Exercise the export with separate private and mounted directories, including
// an old dangling host symlink and k3s recreating its private default symlink.
func TestServerKubeconfigExportReplacesAbsoluteSymlink(t *testing.T) {
	body, err := os.ReadFile("image/server-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	start := strings.Index(script, "    sed ")
	if start < 0 {
		t.Fatal("missing kubeconfig export command")
	}
	end := strings.Index(script[start:], "\n    # A scheduled metrics-server")
	if end < 0 {
		t.Fatal("missing kubeconfig export block")
	}
	export := script[start : start+end]
	for _, shell := range []string{"bash", "bashy"} {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s unavailable", shell)
			}
			for _, port := range []string{"16443", "26443"} {
				t.Run(port, func(t *testing.T) {
					dir := t.TempDir()
					mounted := t.TempDir()
					internal := filepath.Join(dir, "k3s-internal.yaml")
					host := KubeconfigPath(mounted)
					original := "clusters:\n- cluster:\n    server: https://192.0.2.10:6443\n    certificate-authority-data: fixture\n"
					if err := os.WriteFile(internal, []byte(original), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("/etc/rancher/k3s/k3s-internal.yaml", host); err != nil {
						t.Fatal(err)
					}
					block := strings.ReplaceAll(export, "/etc/rancher/k3s", dir)
					block = strings.ReplaceAll(block, "/run/outpost/kubeconfig", mounted)
					cmd := exec.Command(bin, "-c", "set -eu\nOUTPOST_API_PORT=6443\nOUTPOST_HOST_API_PORT="+port+"\n"+block)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("export: %v\n%s", err, out)
					}
					// k3s can recreate its default symlink after export.
					if err := os.Symlink(internal, filepath.Join(dir, "k3s.yaml")); err != nil {
						t.Fatal(err)
					}
					info, err := os.Lstat(host)
					if err != nil {
						t.Fatal(err)
					}
					if !info.Mode().IsRegular() {
						t.Fatalf("mounted kubeconfig must be a regular file, got %s", info.Mode())
					}
					got, err := os.ReadFile(host)
					if err != nil {
						t.Fatal(err)
					}
					want := strings.ReplaceAll(original, "https://192.0.2.10:6443", "https://127.0.0.1:"+port)
					if string(got) != want {
						t.Fatalf("host kubeconfig = %q, want %q", got, want)
					}
					got, err = os.ReadFile(internal)
					if err != nil || string(got) != original {
						t.Fatalf("internal kubeconfig changed: %q, %v", got, err)
					}
				})
			}
		})
	}
}

// renderPublishToml runs the entrypoint's "apiserver publisher" section under
// the given environment and returns the frpc-publish.toml it writes.
func renderPublishToml(t *testing.T, env ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("image", "server-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "# ------------------------------------------------- apiserver publisher ----")
	if !ok {
		t.Fatal("missing apiserver publisher section")
	}
	section, _, ok := strings.Cut(rest, "# --------------------------------------------------------- k3s server ----")
	if !ok {
		t.Fatal("missing end of apiserver publisher section")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "frpc-publish.toml")
	section = strings.ReplaceAll(section, "/tmp/frpc-publish.toml", out)
	script := "set -eu\nlog() { :; }\n" + section
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"OUTPOST_TUNNEL_PORT=7000", "OUTPOST_TUNNEL_TOKEN=tok",
		"OUTPOST_STCP_SECRET=sec", "OUTPOST_API_PORT=6443",
	}, env...)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render: %v\n%s", err, o)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// The federation host publishes the Karmada apiserver as a SECOND stcp proxy
// on the existing frps — iff OUTPOST_FEDERATION_PUBLISH=1. An unfederated
// plane's toml must be byte-for-byte what it was.
func TestServerPublishKarmadaProxyIffFederationEnv(t *testing.T) {
	plain := renderPublishToml(t)
	if strings.Contains(plain, "karmada-apiserver") {
		t.Fatalf("unfederated toml carries the karmada proxy:\n%s", plain)
	}
	if !strings.Contains(plain, `name = "k3s-apiserver"`) {
		t.Fatalf("k3s-apiserver proxy missing:\n%s", plain)
	}
	for _, off := range []string{"", "0", "true"} {
		if got := renderPublishToml(t, "OUTPOST_FEDERATION_PUBLISH="+off); strings.Contains(got, "karmada-apiserver") {
			t.Fatalf("OUTPOST_FEDERATION_PUBLISH=%q must not publish karmada:\n%s", off, got)
		}
	}

	fed := renderPublishToml(t, "OUTPOST_FEDERATION_PUBLISH=1")
	for _, want := range []string{
		`name = "k3s-apiserver"`,
		`name = "karmada-apiserver"`,
		`secretKey = "sec"`,
		`localPort = 15443`,
		`localIP = "127.0.0.1"`,
		`type = "stcp"`,
	} {
		if !strings.Contains(fed, want) {
			t.Errorf("federated toml missing %q:\n%s", want, fed)
		}
	}
	if n := strings.Count(fed, "[[proxies]]"); n != 2 {
		t.Errorf("want exactly 2 proxies, got %d:\n%s", n, fed)
	}

	custom := renderPublishToml(t, "OUTPOST_FEDERATION_PUBLISH=1", "OUTPOST_FEDERATION_PORT=25443")
	if !strings.Contains(custom, "localPort = 25443") {
		t.Errorf("OUTPOST_FEDERATION_PORT not honoured:\n%s", custom)
	}
}

func TestFederationPublishRecreatesAndExportsEnv(t *testing.T) {
	base := ServerOptions{AgentName: "h", TunnelToken: "t", STCPSecret: "s", KubeconfigDir: "/k"}
	fed := base
	fed.FederationPublish = true
	a, err := serverFingerprint(context.Background(), "/nonexistent-podman", base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := serverFingerprint(context.Background(), "/nonexistent-podman", fed)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("toggling FederationPublish must change the fingerprint so the container is recreated")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	stub := filepath.Join(dir, "podman-stub")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\ncase \"$1\" in\n  image) echo sha256:stub ;;\n  inspect) exit 1 ;;\n  run) echo cid ;;\nesac\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{false, true} {
		_ = os.Remove(log)
		o := base
		o.KubeconfigDir = filepath.Join(dir, "kube")
		o.PodmanBin = stub
		o.FederationPublish = on
		if err := UpServer(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(log)
		has := strings.Contains(string(b), "OUTPOST_FEDERATION_PUBLISH=1") && strings.Contains(string(b), "OUTPOST_FEDERATION_PORT=15443")
		if has != on {
			t.Fatalf("federation=%v but env present=%v:\n%s", on, has, b)
		}
		if strings.Contains(string(b), ":15443:") {
			t.Fatalf("the federation port must never be host-published on the control plane:\n%s", b)
		}
	}
}

func TestKarmadaPublisherBackoffAndKubeconfig(t *testing.T) {
	helper := filepath.Join("image", "karmada-publisher.sh")
	out, err := exec.Command("/bin/sh", "-c",
		`OUTPOST_PUBLISHER_LIB_ONLY=1 . "$1"; b=2; for i in 1 2 3 4 5 6; do printf '%s ' $b; b=$(next_backoff $b); done`, "sh", helper).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "2 4 8 16 30 30" {
		t.Fatalf("backoff schedule = %q, want 2 4 8 16 30 30", got)
	}
	body, _ := os.ReadFile(helper)
	for _, want := range []string{"/etc/rancher/k3s/k3s-internal.yaml", "port-forward", "--address 127.0.0.1", "svc/$SVC", "karmada-system"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("publisher missing %q", want)
		}
	}
	if strings.Contains(string(body), "/etc/rancher/k3s/k3s.yaml") {
		t.Error("publisher must not use the host-port-rewritten k3s.yaml")
	}
	ep, _ := os.ReadFile(filepath.Join("image", "server-entrypoint.sh"))
	if !strings.Contains(string(ep), "PUBLISHER_PID") || !strings.Contains(string(ep), `kill -0 "$PUBLISHER_PID"`) {
		t.Error("publisher pid must join the liveness set")
	}
}
