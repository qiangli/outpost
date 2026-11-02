package admincore

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestServiceSSOSecretPreservesOperatorChanges(t *testing.T) {
	core, path := newTestCore(t)
	boot, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := core.SetBuiltins(context.Background(), BuiltinsParams{Cluster: &on, ClusterAgent: &on}); err != nil {
		t.Fatal(err)
	}
	secret, err := core.SSOSecret("apps")
	if err != nil {
		t.Fatal(err)
	}
	got, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ClusterOn() || !got.Cluster.Runtimes.Agent {
		t.Fatal("service publication erased operator toggles")
	}
	if got.AgentName != boot.AgentName || got.Token != boot.Token {
		t.Fatal("service publication changed host identity")
	}
	again, err := core.SSOSecret("apps")
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || secret != again || got.ServiceSSO["apps"] != secret {
		t.Fatal("service key not stable/persisted")
	}
}

func TestServiceSSOSecretConcurrentOperatorChanges(t *testing.T) {
	core, path := newTestCore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			on := true
			_, err := core.SetBuiltins(context.Background(), BuiltinsParams{Cluster: &on, ClusterAgent: &on})
			errs <- err
		}()
		go func(i int) { defer wg.Done(); _, err := core.SSOSecret(fmt.Sprintf("service-%d", i)); errs <- err }(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := conf.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ClusterOn() || !got.Cluster.Runtimes.Agent || len(got.ServiceSSO) != 20 {
		t.Fatal("concurrent mutation lost a toggle or service key")
	}
}

func TestServiceSSOSecretRefusesMalformedConfig(t *testing.T) {
	core, path := newTestCore(t)
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := core.SSOSecret("apps"); err == nil {
		t.Fatal("malformed config was replaced")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{broken" {
		t.Fatal("failed update changed evidence")
	}
}
