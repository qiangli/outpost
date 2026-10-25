package admincore

import (
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// The top-level name "u" is reserved for the public post viewer namespace
// and must be refused like the other cloudbox-side reservations, in any
// letter case; near-misses stay usable.
func TestValidateApp_ReservedNameU(t *testing.T) {
	for _, name := range []string{"u", "U", " u "} {
		ac := conf.AppConfig{Name: name, Port: 8080}
		err := ValidateApp(&ac)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("ValidateApp(%q): want reserved-name error, got %v", name, err)
		}
	}
	if !isReserved("u") {
		t.Error(`isReserved("u") = false, want true`)
	}
	for _, name := range []string{"uu", "u2", "users"} {
		ac := conf.AppConfig{Name: name, Port: 8080}
		if err := ValidateApp(&ac); err != nil {
			t.Errorf("ValidateApp(%q): unexpected error %v", name, err)
		}
	}
}

// The top-level name "v2" is reserved for the cloudbox registry origin (/v2/*)
// and must be refused like the other cloudbox-side reservations, in any
// letter case; near-misses stay usable.
func TestValidateApp_ReservedNameV2(t *testing.T) {
	for _, name := range []string{"v2", "V2", " v2 "} {
		ac := conf.AppConfig{Name: name, Port: 8080}
		err := ValidateApp(&ac)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("ValidateApp(%q): want reserved-name error, got %v", name, err)
		}
	}
	if !isReserved("v2") {
		t.Error(`isReserved("v2") = false, want true`)
	}
	for _, name := range []string{"v22", "v2app", "v3"} {
		ac := conf.AppConfig{Name: name, Port: 8080}
		if err := ValidateApp(&ac); err != nil {
			t.Errorf("ValidateApp(%q): unexpected error %v", name, err)
		}
	}
}

// Public front-door pages and installer redirects are cloudbox-owned root
// routes, so an app must not claim the corresponding top-level name.
func TestValidateApp_ReservedInstallFrontDoorNames(t *testing.T) {
	for _, reserved := range []string{"about", "contact", "install", "install.sh", "install.ps1"} {
		t.Run(reserved, func(t *testing.T) {
			for _, name := range []string{reserved, strings.ToUpper(reserved), " " + reserved + " "} {
				ac := conf.AppConfig{Name: name, Port: 8080}
				err := ValidateApp(&ac)
				if err == nil || !strings.Contains(err.Error(), "reserved") {
					t.Errorf("ValidateApp(%q): want reserved-name error, got %v", name, err)
				}
			}
			if !isReserved(reserved) {
				t.Errorf("isReserved(%q) = false, want true", reserved)
			}
		})
	}

	for _, name := range []string{"about-us", "contacts", "installer", "install.sh.backup", "install.ps10"} {
		ac := conf.AppConfig{Name: name, Port: 8080}
		if err := ValidateApp(&ac); err != nil {
			t.Errorf("ValidateApp(%q): unexpected error %v", name, err)
		}
	}
}
