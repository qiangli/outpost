package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureClusterTunnelToken_GeneratesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	fc := &FileConfig{AgentName: "host-a", Cluster: &ClusterConfig{}}

	tok, err := EnsureClusterTunnelToken(path, fc)
	if err != nil {
		t.Fatalf("EnsureClusterTunnelToken: %v", err)
	}
	if len(tok) < 32 {
		t.Fatalf("token too short (%d chars) to be a credential in front of an apiserver", len(tok))
	}

	// It must survive a restart, or every boot would mint a new secret and
	// every worker would be locked out until it was re-distributed.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	reloaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Cluster == nil || reloaded.Cluster.TunnelToken != tok {
		t.Fatalf("token did not persist; file was: %s", raw)
	}
}

// Idempotent: a second call must return the SAME token. Rotating on every
// call would silently break workers on each daemon restart.
func TestEnsureClusterTunnelToken_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	fc := &FileConfig{Cluster: &ClusterConfig{}}
	first, err := EnsureClusterTunnelToken(path, fc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureClusterTunnelToken(path, fc)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("token rotated between calls: %s then %s", first, second)
	}
}

// An operator pre-seeding one token across hosts must not have it overwritten
// — generation fills a blank, it does not own the field.
func TestEnsureClusterTunnelToken_PreservesOperatorValue(t *testing.T) {
	const seeded = "0123456789abcdef0123456789abcdef0123456789abcdef"
	fc := &FileConfig{Cluster: &ClusterConfig{TunnelToken: seeded}}
	got, err := EnsureClusterTunnelToken("", fc)
	if err != nil {
		t.Fatal(err)
	}
	if got != seeded {
		t.Fatalf("clobbered the operator's token: %s", got)
	}
}

func TestEnsureClusterTunnelToken_NoClusterConfig(t *testing.T) {
	if _, err := EnsureClusterTunnelToken("", &FileConfig{}); err == nil {
		t.Error("expected an error with no cluster config")
	}
	if _, err := EnsureClusterTunnelToken("", nil); err == nil {
		t.Error("expected an error with a nil config")
	}
}

// The default must be LOOPBACK. A control plane that started listening on
// every interface the moment it was enabled would silently expose a tunnel
// in front of an apiserver on whatever network a laptop happened to join.
func TestTunnelBind_DefaultsToLoopback(t *testing.T) {
	addr, port := (&ClusterConfig{}).TunnelBind()
	if addr != "127.0.0.1" {
		t.Errorf("default bind addr = %q, want loopback", addr)
	}
	if port != DefaultTunnelBindPort {
		t.Errorf("default port = %d, want %d", port, DefaultTunnelBindPort)
	}
	// nil must behave like empty rather than panic — callers reach this
	// through fc.Cluster, which is nil on an unconfigured host.
	if a, p := (*ClusterConfig)(nil).TunnelBind(); a != "127.0.0.1" || p != DefaultTunnelBindPort {
		t.Errorf("nil config gave %s:%d", a, p)
	}
}

func TestTunnelBind_HonorsOverrides(t *testing.T) {
	addr, port := (&ClusterConfig{TunnelBindAddr: "0.0.0.0", TunnelBindPort: 7100}).TunnelBind()
	if addr != "0.0.0.0" || port != 7100 {
		t.Errorf("got %s:%d, want 0.0.0.0:7100", addr, port)
	}
}
