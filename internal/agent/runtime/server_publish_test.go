//go:build !windows

package runtime

import (
	"context"
	"os"
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
