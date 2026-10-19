package portal

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "outpost-portal-tests-")
	if err != nil {
		panic("portal test setup: mkdir temp: " + err.Error())
	}
	_ = os.Setenv("XDG_CONFIG_HOME", tmp)
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
