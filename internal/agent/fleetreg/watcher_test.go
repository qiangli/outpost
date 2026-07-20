package fleetreg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/fleet"
)

// testWatcher pins the watcher to a scratch registry and a fake skill store.
func testWatcher(t *testing.T, url string) (*Watcher, *fleet.Catalog) {
	t.Helper()
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	w, err := New(Config{
		CloudboxURL: url, AgentName: "host-a", AccessToken: "tok",
		Catalog: func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) },
		Skills:  func() []string { return []string{"conductor"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, cat
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{AgentName: "h"}); err == nil {
		t.Fatal("CloudboxURL is required")
	}
	if _, err := New(Config{CloudboxURL: "https://x"}); err == nil {
		t.Fatal("AgentName is required")
	}
	w, err := New(Config{CloudboxURL: "https://x/", AgentName: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if w.cfg.CloudboxURL != "https://x" {
		t.Fatalf("trailing slash not trimmed: %q", w.cfg.CloudboxURL)
	}
}

// The snapshot reports the three nouns, and an agent carries its BINDING —
// a peer deciding whether to send work here needs the model, not just a
// nickname.
func TestSnapshotReportsBindings(t *testing.T) {
	w, cat := testWatcher(t, "https://x")
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}

	got := w.Snapshot()
	byKind := map[string][]Asset{}
	for _, a := range got {
		byKind[a.Kind] = append(byKind[a.Kind], a)
	}
	if len(byKind["tool"]) == 0 || len(byKind["agent"]) == 0 || len(byKind["skill"]) != 1 {
		t.Fatalf("snapshot = %+v", got)
	}

	var found bool
	for _, a := range byKind["agent"] {
		if a.Name == "007" {
			found = true
			// The binding is canonicalized to the version-explicit model name:
			// the family alias "fable" resolves to the highest version "fable5",
			// so a peer receives the address it can route to, not a floating
			// pointer. (See coreutils/pkg/fleet family-alias derivation.)
			if a.Detail != "claude:fable5" {
				t.Fatalf("agent detail = %q, want the canonicalized binding claude:fable5", a.Detail)
			}
		}
	}
	if !found {
		t.Fatal("the minted agent is missing from the snapshot")
	}
}

// A function kit is not something this host can be asked to launch, so
// reporting one would advertise a capability that does not exist.
func TestSnapshotSkipsNonCLITools(t *testing.T) {
	w, cat := testWatcher(t, "https://x")
	if err := cat.SaveTool(fleet.Tool{Name: "aikit", Kind: fleet.ToolKindFunc}); err != nil {
		t.Fatal(err)
	}
	for _, a := range w.Snapshot() {
		if a.Name == "aikit" {
			t.Fatal("a function kit was reported as a tool")
		}
	}
}

// A tool that cannot select a model says so, because a peer routing a
// binding here needs to know before it tries.
func TestSnapshotFlagsModellessTools(t *testing.T) {
	w, cat := testWatcher(t, "https://x")
	if err := cat.SaveTool(fleet.Tool{
		Name: "dumb", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "dumb", Launch: fleet.ToolLaunch{Exec: "dumb {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, a := range w.Snapshot() {
		if a.Name == "dumb" && !strings.Contains(a.Detail, "cannot select a model") {
			t.Fatalf("detail = %q", a.Detail)
		}
	}
}

// The hash is order-independent and timestamp-free: an unchanged host must
// hash the same across polls, or the fast path never fires.
func TestContentHashIsStableAndOrderIndependent(t *testing.T) {
	a := []Asset{{Kind: "tool", Name: "codex"}, {Kind: "agent", Name: "007", Detail: "claude:fable"}}
	b := []Asset{{Kind: "agent", Name: "007", Detail: "claude:fable"}, {Kind: "tool", Name: "codex"}}
	if ContentHash(a) != ContentHash(b) {
		t.Fatal("push order changed the hash")
	}
	if ContentHash(a) == ContentHash(nil) {
		t.Fatal("an empty inventory must not collide with a populated one")
	}
	// A changed binding is a changed inventory.
	c := []Asset{{Kind: "tool", Name: "codex"}, {Kind: "agent", Name: "007", Detail: "claude:opus"}}
	if ContentHash(a) == ContentHash(c) {
		t.Fatal("the hash ignores the binding")
	}
}

func TestPushSendsBearerAndPayload(t *testing.T) {
	var gotAuth, gotPath string
	var body payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"ok":true,"applied":"replaced"}`))
	}))
	defer srv.Close()

	w, _ := testWatcher(t, srv.URL)
	assets := w.Snapshot()
	applied, err := w.Push(context.Background(), assets, ContentHash(assets))
	if err != nil {
		t.Fatal(err)
	}
	if applied != "replaced" {
		t.Fatalf("applied = %q", applied)
	}
	if gotAuth != "Bearer tok" || gotPath != registryPath {
		t.Fatalf("auth=%q path=%q", gotAuth, gotPath)
	}
	if body.AgentName != "host-a" || body.ContentHash == "" || len(body.Assets) == 0 {
		t.Fatalf("payload = %+v", body)
	}
}

// 4xx is fatal: retrying a revoked token or a missing scope can never help.
// 5xx is not.
func TestPushErrorFatality(t *testing.T) {
	for _, tt := range []struct {
		status int
		fatal  bool
	}{{401, true}, {403, true}, {404, true}, {400, true}, {500, false}, {503, false}} {
		e := &PushError{Status: tt.status}
		if e.Fatal() != tt.fatal {
			t.Errorf("status %d: Fatal() = %v", tt.status, e.Fatal())
		}
	}
}

func TestPushReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"scope missing"}`))
	}))
	defer srv.Close()

	w, _ := testWatcher(t, srv.URL)
	_, err := w.Push(context.Background(), nil, "h")
	var pe *PushError
	if !asPushError(err, &pe) || pe.Status != 403 || !strings.Contains(pe.Body, "scope missing") {
		t.Fatalf("err = %v", err)
	}
	if !pe.Fatal() {
		t.Fatal("403 must be fatal")
	}
}

// The loop pushes once on start, then only when the inventory changes or the
// heartbeat falls due.
func TestRunPushesOnChangeAndHeartbeat(t *testing.T) {
	var pushes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pushes++
		_, _ = w.Write([]byte(`{"ok":true,"applied":"replaced"}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	w, err := New(Config{
		CloudboxURL: srv.URL, AgentName: "host-a",
		PollInterval: 5 * time.Millisecond, HeartbeatInterval: time.Hour,
		Catalog: func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) },
		Skills:  func() []string { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if pushes != 1 {
		t.Fatalf("pushes = %d — an unchanged inventory must push once, not on every tick", pushes)
	}
}
