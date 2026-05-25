package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// fakeCloudbox stands in for cloudbox's /api/hosts/:host/apps/:name/grants
// surface. Captures the last POST/DELETE body, lets the test drive
// status codes, and tracks the call count per method so cache-related
// assertions are tractable.
type fakeCloudbox struct {
	mu        sync.Mutex
	posts     int
	deletes   int
	gets      int
	lastPost  string
	delEmails []string
	getStatus int
	getBody   string
	postBody  string
}

func newFakeCloudbox() *fakeCloudbox {
	return &fakeCloudbox{
		getStatus: http.StatusOK,
		getBody:   `[]`,
		postBody:  `{"ok":true}`,
	}
}

func (f *fakeCloudbox) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/hosts/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			f.gets++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.getStatus)
			_, _ = io.WriteString(w, f.getBody)
		case http.MethodPost:
			f.posts++
			b, _ := io.ReadAll(r.Body)
			f.lastPost = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, f.postBody)
		case http.MethodDelete:
			f.deletes++
			// Path is /api/hosts/<host>/apps/<app>/grants/<email>
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/hosts/"), "/")
			if len(parts) >= 5 && parts[3] == "grants" {
				f.delEmails = append(f.delEmails, parts[4])
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

// newTestProvisionRouter mounts the provisioning routes on a gin engine
// pointed at the fake cloudbox. Returns the bound httptest.Server URL
// so tests can hit it directly.
func newTestProvisionRouter(t *testing.T, reg *AppRegistry, cloudbox string, accessToken, agentName string) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	RegisterProvisionRoutes(eng, ProvisionDeps{
		Apps:         reg,
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
		CloudboxBase: cloudbox,
		AccessToken:  accessToken,
		AgentName:    agentName,
	})
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)
	return srv
}

// TestProvision_TokenValidation covers the auth surface: missing bearer
// → 401, unknown bearer → 401, mismatched URL :name → 400.
func TestProvision_TokenValidation(t *testing.T) {
	cb := newFakeCloudbox()
	cbSrv := httptest.NewServer(cb.handler())
	t.Cleanup(cbSrv.Close)

	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "grafana", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, RequireLogin: true, TrustCloudIdentity: true,
		ProvisioningToken: "secret-token-grafana",
	})
	mustRegister(t, reg, conf.AppConfig{
		Name: "forgejo", Scheme: "http", Host: "127.0.0.1", Port: 9998,
		Enabled: true, RequireLogin: true, TrustCloudIdentity: true,
		ProvisioningToken: "secret-token-forgejo",
	})

	srv := newTestProvisionRouter(t, reg, cbSrv.URL, "outpost-access-token", "myhost")

	t.Run("missing bearer rejected", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/grafana/users", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("unknown token rejected", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/grafana/users", nil)
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("token for wrong app on path rejected", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/grafana/users", nil)
		req.Header.Set("Authorization", "Bearer secret-token-forgejo")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("valid token + matching app passes", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/grafana/users", nil)
		req.Header.Set("Authorization", "Bearer secret-token-grafana")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
}

// TestProvision_Unpaired_Returns503 confirms the relay refuses to call
// cloudbox when outpost isn't paired (no access token or no cloudbox
// base URL). The operator sees a clear 503 with the misconfiguration
// reason rather than a confusing 502 from a missing cloudbox.
func TestProvision_Unpaired_Returns503(t *testing.T) {
	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "grafana", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, ProvisioningToken: "tk",
	})
	// AgentName + CloudboxBase + AccessToken all empty = unpaired.
	srv := newTestProvisionRouter(t, reg, "", "", "")

	req, _ := http.NewRequest("POST", srv.URL+"/_periscope/apps/grafana/users",
		strings.NewReader(`{"email":"alice@example.com"}`))
	req.Header.Set("Authorization", "Bearer tk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestProvision_UpsertForwardsToCloudbox confirms POST is relayed
// to cloudbox with the outpost's access_token, the body is forwarded
// unchanged, and the email is lowercased.
func TestProvision_UpsertForwardsToCloudbox(t *testing.T) {
	cb := newFakeCloudbox()
	cbSrv := httptest.NewServer(cb.handler())
	t.Cleanup(cbSrv.Close)

	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "app", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, ProvisioningToken: "tk",
	})
	srv := newTestProvisionRouter(t, reg, cbSrv.URL, "outpost-tk", "myhost")

	body := `{"email":"  ALICE@example.com  ","name":"Alice","role":"admin"}`
	req, _ := http.NewRequest("POST", srv.URL+"/_periscope/apps/app/users",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.posts != 1 {
		t.Errorf("cloudbox saw %d POSTs, want 1", cb.posts)
	}
	var got provisionGrant
	if err := json.Unmarshal([]byte(cb.lastPost), &got); err != nil {
		t.Fatalf("cloudbox body not json: %v (%q)", err, cb.lastPost)
	}
	if got.Email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com (normalized)", got.Email)
	}
	if got.Name != "Alice" {
		t.Errorf("name = %q, want Alice", got.Name)
	}
	if got.Role != "admin" {
		t.Errorf("role = %q, want admin", got.Role)
	}
}

// TestProvision_UpsertEmailRequired: POST without email is 400 and
// cloudbox is never called.
func TestProvision_UpsertEmailRequired(t *testing.T) {
	cb := newFakeCloudbox()
	cbSrv := httptest.NewServer(cb.handler())
	t.Cleanup(cbSrv.Close)

	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "app", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, ProvisioningToken: "tk",
	})
	srv := newTestProvisionRouter(t, reg, cbSrv.URL, "outpost-tk", "myhost")

	req, _ := http.NewRequest("POST", srv.URL+"/_periscope/apps/app/users",
		strings.NewReader(`{"name":"alice"}`))
	req.Header.Set("Authorization", "Bearer tk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	cb.mu.Lock()
	if cb.posts != 0 {
		t.Errorf("cloudbox saw %d POSTs on email-missing call, want 0", cb.posts)
	}
	cb.mu.Unlock()
}

// TestProvision_GetCacheTTL: a hot GET should not refetch from cloudbox.
// A subsequent POST should invalidate the cache so the next GET refetches.
func TestProvision_GetCacheTTL(t *testing.T) {
	cb := newFakeCloudbox()
	cbSrv := httptest.NewServer(cb.handler())
	t.Cleanup(cbSrv.Close)

	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "app", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, ProvisioningToken: "tk",
	})
	// Clear any stale entry from prior tests sharing the global cache.
	provisionCacheFor(reg).invalidate("app")

	srv := newTestProvisionRouter(t, reg, cbSrv.URL, "outpost-tk", "myhost")

	hit := func() {
		req, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/app/users", nil)
		req.Header.Set("Authorization", "Bearer tk")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	hit()
	hit()
	hit()
	cb.mu.Lock()
	if cb.gets != 1 {
		t.Errorf("cloudbox saw %d GETs, want 1 (cache should absorb 2 of 3)", cb.gets)
	}
	cb.mu.Unlock()

	// POST invalidates.
	post, _ := http.NewRequest("POST", srv.URL+"/_periscope/apps/app/users",
		strings.NewReader(`{"email":"a@b.com"}`))
	post.Header.Set("Authorization", "Bearer tk")
	post.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	hit()
	cb.mu.Lock()
	if cb.gets != 2 {
		t.Errorf("cloudbox saw %d GETs total, want 2 (POST should have invalidated)", cb.gets)
	}
	cb.mu.Unlock()
}

// TestProvision_DeleteForwardsAndInvalidatesCache: DELETE :email path
// reaches cloudbox at /grants/<email> and invalidates the cache.
func TestProvision_DeleteForwardsAndInvalidatesCache(t *testing.T) {
	cb := newFakeCloudbox()
	cbSrv := httptest.NewServer(cb.handler())
	t.Cleanup(cbSrv.Close)

	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "app", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, ProvisioningToken: "tk",
	})
	provisionCacheFor(reg).invalidate("app")

	srv := newTestProvisionRouter(t, reg, cbSrv.URL, "outpost-tk", "myhost")

	// Warm the cache.
	hit := func() {
		req, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/app/users", nil)
		req.Header.Set("Authorization", "Bearer tk")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	hit()
	hit()
	cb.mu.Lock()
	startingGets := cb.gets
	cb.mu.Unlock()

	// DELETE alice (uppercase in URL should lowercase before forward).
	del, _ := http.NewRequest("DELETE",
		srv.URL+"/_periscope/apps/app/users/ALICE@example.com", nil)
	del.Header.Set("Authorization", "Bearer tk")
	resp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	cb.mu.Lock()
	if cb.deletes != 1 {
		t.Errorf("cloudbox DELETEs = %d, want 1", cb.deletes)
	}
	if len(cb.delEmails) != 1 || cb.delEmails[0] != "alice@example.com" {
		t.Errorf("delete email = %v, want [alice@example.com]", cb.delEmails)
	}
	cb.mu.Unlock()

	hit()
	cb.mu.Lock()
	if cb.gets != startingGets+1 {
		t.Errorf("DELETE should have invalidated cache; gets = %d, want %d",
			cb.gets, startingGets+1)
	}
	cb.mu.Unlock()
}

// TestProvision_StaleWhileError: when cloudbox returns 500 after a
// previous successful GET, the relay should serve the stale cached body
// rather than failing the app's reconcile loop.
func TestProvision_StaleWhileError(t *testing.T) {
	cb := newFakeCloudbox()
	cb.getBody = `[{"email":"a@b.com"}]`
	cbSrv := httptest.NewServer(cb.handler())
	t.Cleanup(cbSrv.Close)

	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "app", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, ProvisioningToken: "tk",
	})
	provisionCacheFor(reg).invalidate("app")

	srv := newTestProvisionRouter(t, reg, cbSrv.URL, "outpost-tk", "myhost")

	// Successful warmup.
	r1, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/app/users", nil)
	r1.Header.Set("Authorization", "Bearer tk")
	resp1, _ := http.DefaultClient.Do(r1)
	b1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if string(b1) != `[{"email":"a@b.com"}]` {
		t.Fatalf("warmup body = %q", b1)
	}

	// Now make cloudbox 5xx AND invalidate the freshness window so the
	// next GET tries to refetch and observes the error.
	cb.mu.Lock()
	cb.getStatus = http.StatusInternalServerError
	cb.getBody = "boom"
	cb.mu.Unlock()
	provisionCacheFor(reg).entries["app"] = grantCacheEntry{
		body:      []byte(`[{"email":"a@b.com"}]`),
		expiresAt: time.Now().Add(-time.Second), // expired
	}

	r2, _ := http.NewRequest("GET", srv.URL+"/_periscope/apps/app/users", nil)
	r2.Header.Set("Authorization", "Bearer tk")
	resp2, _ := http.DefaultClient.Do(r2)
	b2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("stale-while-error status = %d, want 200", resp2.StatusCode)
	}
	if string(b2) != `[{"email":"a@b.com"}]` {
		t.Errorf("stale body = %q, want previously-cached payload", b2)
	}
}

// TestProvision_LookupByProvisioningTokenAcrossModeSwap: an HTTP→TCP
// re-register on the same name must move the token with it, not strand
// it under the old mode.
func TestProvision_LookupByProvisioningTokenAcrossModeSwap(t *testing.T) {
	reg := NewAppRegistry()
	mustRegister(t, reg, conf.AppConfig{
		Name: "app", Scheme: "http", Host: "127.0.0.1", Port: 9999,
		Enabled: true, ProvisioningToken: "alpha",
	})
	if got, ok := reg.LookupByProvisioningToken("alpha"); !ok || got != "app" {
		t.Errorf("LookupByProvisioningToken alpha = (%q, %v), want (app, true)", got, ok)
	}
	// Swap to tcp with a different token.
	mustRegister(t, reg, conf.AppConfig{
		Name: "app", Scheme: "tcp", Host: "127.0.0.1", Port: 22,
		Enabled: true, ProvisioningToken: "beta",
	})
	if _, ok := reg.LookupByProvisioningToken("alpha"); ok {
		t.Errorf("old token should not resolve after mode swap")
	}
	if got, ok := reg.LookupByProvisioningToken("beta"); !ok || got != "app" {
		t.Errorf("new token = (%q, %v), want (app, true)", got, ok)
	}
	reg.Unregister("app")
	if _, ok := reg.LookupByProvisioningToken("beta"); ok {
		t.Errorf("token should not resolve after Unregister")
	}
}

func mustRegister(t *testing.T, reg *AppRegistry, ac conf.AppConfig) {
	t.Helper()
	if err := reg.RegisterFromConfig(ac); err != nil {
		t.Fatalf("register %q: %v", ac.Name, err)
	}
}
