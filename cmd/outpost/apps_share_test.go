package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestAppsShareCommands(t *testing.T) {
	var posts, deletes, gets int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer attachment-token" {
			t.Errorf("wrong token")
		}
		if r.URL.Path != "/api/v1/hosts/attached/shares" && r.URL.Path != "/api/v1/hosts/attached/shares/share-1" {
			t.Errorf("path = %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			gets++
			json.NewEncoder(w).Encode(map[string]any{"shares": []any{map[string]any{"id": "share-1", "host": "attached", "app": "notes", "email": "member@example.test", "created_at": "2026-01-02T03:04:05Z"}}})
		case http.MethodPost:
			posts++
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"share":{"id":"share-1","host":"attached","app":"notes","email":"member@example.test","created_at":"2026-01-02T03:04:05Z"}}`))
		case http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := conf.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	fc := &conf.FileConfig{AgentName: "legacy", PortalURL: "http://invalid.example", AccessToken: "legacy-token", Attachments: []conf.SatelliteAttachment{{Endpoint: server.URL, Host: "attached", Token: "attachment-token"}}}
	if err := conf.SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := appsCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := run("share", "notes", "--email", "member@example.test", "--dry-run", "--json"); !strings.Contains(got, `"dry_run":true`) {
		t.Fatal(got)
	}
	if posts != 0 || gets != 0 {
		t.Fatalf("dry run sent requests")
	}
	if got := run("share", "notes", "--email", "member@example.test", "--json"); !strings.Contains(got, `"share"`) {
		t.Fatal(got)
	}
	if got := run("shares", "notes", "--json"); !strings.Contains(got, `"shares"`) {
		t.Fatal(got)
	}
	if got := run("unshare", "notes", "--email", "member@example.test", "--dry-run", "--json"); !strings.Contains(got, `"dry_run":true`) {
		t.Fatal(got)
	}
	if deletes != 0 {
		t.Fatal("dry-run deleted share")
	}
	run("unshare", "notes", "--email", "member@example.test", "--json")
	if posts != 1 || deletes != 1 || gets != 3 {
		t.Fatalf("posts=%d deletes=%d gets=%d", posts, deletes, gets)
	}
}

func TestShareTargetSelection(t *testing.T) {
	if _, err := shareTargetFromConfig(&conf.FileConfig{}); err == nil {
		t.Fatal("unpaired accepted")
	}
	got, err := shareTargetFromConfig(&conf.FileConfig{AgentName: "old", PortalURL: "https://portal.example", AccessToken: "legacy"})
	if err != nil || got.host != "old" || got.token != "legacy" || got.endpoint != "https://portal.example" {
		t.Fatalf("target=%+v err=%v", got, err)
	}
	if _, err := shareTargetFromConfig(&conf.FileConfig{AgentName: "old", AccessToken: "legacy", Attachments: []conf.SatelliteAttachment{{Host: "new"}}}); err == nil {
		t.Fatal("incomplete attachment silently fell back")
	}
}

func TestAppsShareRequiresExactlyOneAudience(t *testing.T) {
	for _, args := range [][]string{{"notes"}, {"notes", "--email", "a@example.test", "--circle", "family"}} {
		cmd := appsShareCmd()
		cmd.SetArgs(args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("args %v: err = %v", args, err)
		}
	}
}
