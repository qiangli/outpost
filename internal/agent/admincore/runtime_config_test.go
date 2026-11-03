package admincore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func runtimeConfigCore(t *testing.T) (*Server, string) {
	t.Helper()
	core, path := newTestCore(t)
	fc, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	fc.AccessToken = "access"
	fc.Cluster = &conf.ClusterConfig{Enabled: &on, ControlPlane: &on, Runtimes: conf.ClusterRuntimes{Agent: true}}
	if err := conf.SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	return core, path
}

func TestRuntimeConfigConcurrentPreservation(t *testing.T) {
	core, path := runtimeConfigCore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 120)
	for i := range 20 {
		wg.Add(6)
		go func() {
			defer wg.Done()
			on := true
			_, err := core.SetBuiltins(context.Background(), BuiltinsParams{Shell: &on, Cluster: &on, ClusterAgent: &on})
			errs <- err
		}()
		go func() { defer wg.Done(); _, err := core.RotateMCPBearer(); errs <- err }()
		go func() { defer wg.Done(); errs <- core.StoreHostCertificate("host1", "access", "cert", "pubkey") }()
		go func() { defer wg.Done(); _, err := core.PrepareControlPlane("/owned/kubeconfig"); errs <- err }()
		go func() {
			defer wg.Done()
			errs <- core.StoreClusterCredential("host1", "access", "https://api", "rotated-token", []byte("ca"))
		}()
		go func(i int) { defer wg.Done(); _, err := core.SSOSecret(fmt.Sprintf("app-%d", i)); errs <- err }(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	fc, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fc.AgentName != "host1" || fc.AccessToken != "access" || fc.Token != "t" || !fc.ClusterOn() || !fc.Cluster.Runtimes.Agent || fc.ShellEnabled == nil || !*fc.ShellEnabled {
		t.Fatal("operator state or identity lost")
	}
	cc := fc.Cluster
	if cc.HostCert != "cert" || cc.CAPubkey != "pubkey" || cc.APIURL != "https://api" || cc.Token != "rotated-token" || string(cc.CA) != "ca" || cc.ControlPlaneKubeconfig != "/owned/kubeconfig" || len(cc.TunnelToken) < 32 || len(cc.STCPSecret) < 32 || len(fc.MCPBearerToken) != 64 || len(fc.ServiceSSO) != 20 {
		t.Fatal("runtime field lost")
	}
	prepared, err := core.PrepareControlPlane("/other")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Cluster.TunnelToken != cc.TunnelToken || prepared.Cluster.STCPSecret != cc.STCPSecret || prepared.Cluster.ControlPlaneKubeconfig != cc.ControlPlaneKubeconfig {
		t.Fatal("prepared secrets/path changed")
	}
	prepared.Cluster.Enabled = nil
	current, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !current.ClusterOn() {
		t.Fatal("returned snapshot aliases persisted state")
	}
}

func TestRuntimeConfigPreservesExplicitControlPlaneValues(t *testing.T) {
	core, path := runtimeConfigCore(t)
	fc, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fc.Cluster.TunnelToken = strings.Repeat("a", 64)
	fc.Cluster.STCPSecret = strings.Repeat("b", 64)
	fc.Cluster.ControlPlaneKubeconfig = "/operator/config"
	if err := conf.SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	got, err := core.PrepareControlPlane("/default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cluster.TunnelToken != fc.Cluster.TunnelToken || got.Cluster.STCPSecret != fc.Cluster.STCPSecret || got.Cluster.ControlPlaneKubeconfig != fc.Cluster.ControlPlaneKubeconfig {
		t.Fatal("operator values replaced")
	}
}

func TestRuntimeConfigRefusesUnreadableOrMissingState(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			core, path := runtimeConfigCore(t)
			if missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := []func() error{
				func() error { _, err := core.RotateMCPBearer(); return err },
				func() error { return core.StoreHostCertificate("host1", "access", "cert", "pub") },
				func() error { _, err := core.PrepareControlPlane("/path"); return err },
				func() error { return core.StoreClusterCredential("host1", "access", "api", "token", []byte("ca")) },
			}
			for _, call := range calls {
				if err := call(); err == nil {
					t.Fatal("invalid state accepted")
				}
			}
			raw, err := os.ReadFile(path)
			if missing {
				if !os.IsNotExist(err) {
					t.Fatal("missing config recreated")
				}
			} else if err != nil || string(raw) != "{broken" {
				t.Fatal("invalid config overwritten")
			}
		})
	}
}

func TestRuntimeConfigCredentialUpdatePreservesMembership(t *testing.T) {
	core, path := runtimeConfigCore(t)
	off := false
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{Cluster: &off}); err != nil {
		t.Fatal(err)
	}
	if err := core.StoreClusterCredential("host1", "access", "api", "token", []byte("ca")); err != nil {
		t.Fatal(err)
	}
	fc, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fc.ClusterOn() || !fc.Cluster.Runtimes.Agent || !fc.Cluster.ControlPlaneOn() {
		t.Fatal("credential refresh changed membership")
	}
	for _, call := range []func() error{
		func() error { return core.StoreHostCertificate("other", "access", "cert", "pub") },
		func() error { return core.StoreClusterCredential("host1", "old-access", "api", "token", []byte("ca")) },
	} {
		if err := call(); err == nil {
			t.Fatal("stale identity accepted")
		}
	}
}

func TestRuntimeConfigRefusesChangedPeerMembership(t *testing.T) {
	core, path := runtimeConfigCore(t)
	fc, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fc.Cluster.JoinEndpoint = "peer.example:6443"
	fc.Cluster.Token = "peer-token"
	if err := conf.SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.StoreClusterCredential("host1", "access", "cloud-api", "cloud-token", []byte("cloud-ca")); err == nil {
		t.Fatal("cloud refresh replaced peer credentials")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("refused refresh modified peer membership")
	}
}
