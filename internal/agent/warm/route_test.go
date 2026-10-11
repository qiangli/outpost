package warm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The /admin/warm route resolves the executor PER REQUEST, because the warm
// plane is a service unit the operator can enable or disable live.
//
// It used to capture the executor at boot and mount nothing when warm serving
// was off — cloudbox then got a 404, indistinguishable from an outpost too
// old to warm anything. Now the route is always mounted and an inactive unit
// answers 503 with the reason.
func TestMountRouteResolvesTheExecutorPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var live *Executor
	r := gin.New()
	MountRoute(r.Group("/"), func() *Executor { return live })

	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/warm",
			strings.NewReader(`{"model":"llama3.2:1b","mode":"load"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	w := post()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("inactive unit = %d %s, want 503", w.Code, w.Body.String())
	}
	var body struct{ Error string }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "warm serving is not active") {
		t.Errorf("the 503 does not say why: %q", body.Error)
	}

	// The operator enables the unit: the SAME mounted route now serves,
	// with no restart and no remount.
	live = New(Config{
		Ollama:    newFakeOllama(),
		Gauge:     &fakeGauge{budget: 32 << 30},
		UsableMem: func() uint64 { return 64 << 30 },
	})
	if w := post(); w.Code != http.StatusOK {
		t.Fatalf("active unit = %d %s, want 200", w.Code, w.Body.String())
	}

	// And disabling it again goes back to the honest 503 rather than
	// continuing to drive an executor the unit no longer owns.
	live = nil
	if w := post(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("re-disabled unit = %d, want 503", w.Code)
	}
}

// A nil provider mounts nothing — the daemon has no warm wiring at all.
func TestMountRouteWithNoProviderMountsNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	MountRoute(r.Group("/"), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/warm", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unmounted route = %d, want 404", w.Code)
	}
}
