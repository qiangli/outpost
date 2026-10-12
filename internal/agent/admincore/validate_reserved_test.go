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
