package upgrade

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// routeHarness mounts the /admin/upgrade handler with a controllable
// worker and returns an httptest.Server in front of it. Token defaults
// to "secret"; tests can override with the field.
type routeHarness struct {
	t      *testing.T
	server *httptest.Server
	worker *Worker
	state  StateSnapshot
}

func newRouteHarness(t *testing.T) *routeHarness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "outpost")
	if err := writeFile(bin, "old"); err != nil {
		t.Fatal(err)
	}
	h := &routeHarness{
		t: t,
		state: StateSnapshot{
			AutoUpgrade:   true,
			CurrentCommit: "abc1234",
			BinaryPath:    bin,
		},
	}
	w, err := NewWorker(Options{
		State:   func() StateSnapshot { return h.state },
		Restart: func() {},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.worker = w

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	MountRoute(engine.Group("/"), "secret", w)
	h.server = httptest.NewServer(engine)
	t.Cleanup(h.server.Close)
	return h
}

func (h *routeHarness) post(token string, body any) *http.Response {
	h.t.Helper()
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/admin/upgrade", bytes.NewReader(buf))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func TestRoute_RejectsMissingBearer(t *testing.T) {
	h := newRouteHarness(t)
	resp := h.post("", Envelope{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRoute_RejectsWrongBearer(t *testing.T) {
	h := newRouteHarness(t)
	resp := h.post("wrong-token", Envelope{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRoute_RejectsInvalidEnvelope(t *testing.T) {
	h := newRouteHarness(t)
	resp := h.post("secret", Envelope{ReleaseID: "r1"}) // missing required fields
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestRoute_DispatchesAndMapsStatus(t *testing.T) {
	h := newRouteHarness(t)
	// Envelope.Commit matches CurrentCommit → 304 same_commit.
	resp := h.post("secret", Envelope{
		ReleaseID: "r1",
		URL:       "https://example.com/x",
		SHA256:    "deadbeef",
		Commit:    "abc1234",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 304, got %d: %s", resp.StatusCode, body)
	}
}

func TestRoute_RejectsBadJSON(t *testing.T) {
	h := newRouteHarness(t)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/admin/upgrade", strings.NewReader("not-json"))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o755)
}
