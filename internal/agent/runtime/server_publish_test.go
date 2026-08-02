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
		"--disable=traefik,servicelb,metrics-server",
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

	dockerfile, err := os.ReadFile(filepath.Join("image", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"ARG METRICS_SERVER_VERSION=v0.8.1",
		"metrics-server-linux-${TARGETARCH}",
		"dd2455a5902d100aab7e5f7f9bcd8f715b6d39f97f161bbe87e3c24f61fa296c",
		"0a3a052c44111dd0cc4ee86729d1eb621dd12b064f4dfb92e012d420ef0fd06f",
		`sha256sum -c -`,
		"COPY metrics-server-supervisor.sh /usr/local/bin/metrics-server-supervisor.sh",
	} {
		if !strings.Contains(string(dockerfile), required) {
			t.Errorf("runtime image missing metrics-server artifact contract %q", required)
		}
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
		"--reuid=65532", "--clear-groups",
		`--kubeconfig="$internal_kubeconfig"`,
		"--kubelet-preferred-address-types=ExternalIP",
		"--kubelet-use-node-status-port", "--kubelet-insecure-tls",
		"CN=system:serviceaccount:kube-system:metrics-server",
		"openssl x509 -req -days 2",
		`-lt 8640`,
		`ip address add "$service_ip/32" dev lo`,
		`TCP-LISTEN:443,bind=${service_ip}`,
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
}
