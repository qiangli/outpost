package admincore

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "outpost-admincore-tests-")
	if err != nil {
		panic("admincore test setup: mkdir temp: " + err.Error())
	}
	_ = os.Setenv("XDG_CONFIG_HOME", tmp)
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
