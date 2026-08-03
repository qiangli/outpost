package runtime

import (
	"io/fs"
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
