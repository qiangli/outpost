package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/yoke/pkg/webconsole"
)

func TestPairedOwnerOperatesAppsThroughTunnelButShareeDoesNot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_APPS_DIR", filepath.Join(t.TempDir(), "apps"))
	t.Setenv("BASHY_APP_OPERATOR", "sharee@example.test") // inherited value must not grant access
	path, err := conf.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := conf.SaveFile(path, &conf.FileConfig{Cloudbox: &conf.CloudboxPairing{
		Host: "paired", Token: "host-token", OwnerEmail: "owner@example.test",
	}}); err != nil {
		t.Fatal(err)
	}
	programs := supervisordPrograms("outpost", "bashy", "", "")
	var operator string
	for _, item := range programs[1].Env {
		if strings.HasPrefix(item, "BASHY_APP_OPERATOR=") {
			operator = strings.TrimPrefix(item, "BASHY_APP_OPERATOR=")
		}
	}
	if operator != "owner@example.test" {
		t.Fatalf("Apps console operator = %q, want paired owner", operator)
	}
	h, closeHandler, err := webconsole.Handler(webconsole.Options{Operators: []string{operator}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	request := func(email, name string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/apps/registered", strings.NewReader(`{"name":"`+name+`","port":8080}`))
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-Prefix", "/matrix/h/paired/app/apps")
		r.Header.Set("Remote-User", email)
		r.Header.Set("Remote-Email", email)
		r.Header.Set("Remote-Groups", "admin") // cloud tier alone cannot grant operator access
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("sharee@example.test", "sharee-app"); got != http.StatusForbidden {
		t.Errorf("sharee tunnel write = %d, want 403", got)
	}
	if got := request("owner@example.test", "owner-app"); got != http.StatusCreated {
		t.Errorf("paired owner tunnel write = %d, want 201", got)
	}
	if err := conf.SaveFile(path, &conf.FileConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, item := range supervisordPrograms("outpost", "bashy", "", "")[1].Env {
		if strings.HasPrefix(item, "BASHY_APP_OPERATOR=") {
			t.Fatalf("unpaired host inherited an operator: %q", item)
		}
	}
}
