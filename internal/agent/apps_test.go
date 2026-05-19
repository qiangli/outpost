package agent

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// TestRegisterFromConfig — happy path plus validation: bad scheme/port
// must error, disabled entries must be silently skipped (so the admin UI
// can keep them in the persisted list without serving them).
func TestRegisterFromConfig(t *testing.T) {
	reg := NewAppRegistry()

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "alpha", Scheme: "http", Host: "127.0.0.1", Port: 9000, Enabled: true,
	}); err != nil {
		t.Fatalf("happy: %v", err)
	}
	if target := reg.LookupTarget("alpha"); target == nil || target.Host != "127.0.0.1:9000" {
		t.Errorf("registered target = %v", target)
	}

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "beta", Scheme: "http", Port: 8000, Enabled: false,
	}); err != nil {
		t.Errorf("disabled entry should be skipped silently, got error: %v", err)
	}
	if reg.LookupTarget("beta") != nil {
		t.Error("disabled app should not be in the live registry")
	}

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "gamma", Scheme: "ftp", Port: 21, Enabled: true,
	}); err == nil {
		t.Error("expected error on unsupported scheme")
	}

	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "delta", Scheme: "http", Port: 0, Enabled: true,
	}); err == nil {
		t.Error("expected error on zero port")
	}
}

// TestRegisterWithRole_DefaultsAndValidation: empty role defaults to
// "user"; a recognised role is preserved verbatim; an unrecognised role
// errors out at register time so misconfigured apps never reach the cloud.
func TestRegisterWithRole_DefaultsAndValidation(t *testing.T) {
	reg := NewAppRegistry()

	if err := reg.RegisterWithRole("a", "http://127.0.0.1:9000", ""); err != nil {
		t.Fatalf("empty role: %v", err)
	}
	if err := reg.RegisterWithRole("b", "http://127.0.0.1:9001", "admin"); err != nil {
		t.Fatalf("admin role: %v", err)
	}
	if err := reg.RegisterWithRole("c", "http://127.0.0.1:9002", "root"); err == nil {
		t.Fatalf("unrecognised role should error")
	}

	got := map[string]string{}
	for _, e := range reg.Entries() {
		got[e.Name] = e.Role
	}
	if got["a"] != "user" {
		t.Errorf("empty role should default to user, got %q", got["a"])
	}
	if got["b"] != "admin" {
		t.Errorf("admin role should be preserved, got %q", got["b"])
	}
	if _, ok := got["c"]; ok {
		t.Errorf("unrecognised role should not have been registered")
	}
}

// TestRegisterFromConfig_RolePropagates: AppConfig.Role flows into the
// registry's Entries() output so /apps publishes the owner's declarations.
func TestRegisterFromConfig_RolePropagates(t *testing.T) {
	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "jupyter", Scheme: "http", Port: 8888, Enabled: true, Role: "admin",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, e := range reg.Entries() {
		if e.Name == "jupyter" && e.Role != "admin" {
			t.Errorf("role on AppConfig should propagate, got %q", e.Role)
		}
	}
}

// TestUnregister removes the entry from the registry; subsequent lookups
// must return nil, and a re-Register with a different target must take.
func TestUnregister(t *testing.T) {
	reg := NewAppRegistry()
	if err := reg.Register("ycode", "http://127.0.0.1:8765"); err != nil {
		t.Fatal(err)
	}
	reg.Unregister("ycode")
	if reg.LookupTarget("ycode") != nil {
		t.Error("Unregister did not clear the entry")
	}
	// Unregister of a missing entry is a no-op.
	reg.Unregister("ycode")
	// Re-register with a different host should stick.
	if err := reg.Register("ycode", "http://127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	if reg.LookupTarget("ycode").Host != "127.0.0.1:9999" {
		t.Errorf("re-register did not take")
	}
}
