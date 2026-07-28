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
