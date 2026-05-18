package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/hostauth"
)

// postAuth POSTs body to the supplied agent engine's /auth and returns
// the status code and decoded response body.
func postAuth(t *testing.T, eng *gin.Engine, body any, headers map[string]string) (int, AuthResponse, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/auth", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		raw, _ := io.ReadAll(w.Body)
		return w.Code, AuthResponse{}, string(raw)
	}
	var resp AuthResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp, ""
}

// TestAuthOSPath covers the host-OS branch:
//   - Right password + matching user → role=admin by default.
//   - Empty admin allowlist keeps the default admin promotion.
//   - Non-empty admin allowlist downgrades non-listed callers to user.
//   - Wrong password → 401.
//   - User mismatch (submitted name ≠ agent's OS user) → 401 even with
//     the right password, so a custom UI can't trick the agent into
//     verifying a different account.
//   - Missing user → 400.
func TestAuthOSPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	currentUser, _ := hostauth.CurrentUser()
	if currentUser == "" {
		t.Skip("no current user")
	}
	stub := hostauth.StubAuth{Want: map[string]string{currentUser: "right"}}

	t.Run("default admin", func(t *testing.T) {
		eng := gin.New()
		RegisterRoutes(eng.Group("/"), Deps{Auth: stub})
		code, body, _ := postAuth(t, eng, map[string]string{"user": currentUser, "password": "right"}, nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if body.Role != "admin" {
			t.Errorf("role = %q, want admin", body.Role)
		}
		if body.User != currentUser {
			t.Errorf("user = %q, want %q", body.User, currentUser)
		}
	})

	t.Run("allowlist downgrades non-listed", func(t *testing.T) {
		eng := gin.New()
		RegisterRoutes(eng.Group("/"), Deps{Auth: stub, Admins: NewAdminSet("only-alice@example.com")})
		code, body, _ := postAuth(t, eng, map[string]string{"user": currentUser, "password": "right"},
			map[string]string{"X-Periscope-User": "bob@example.com"})
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if body.Role != "user" {
			t.Errorf("non-listed role = %q, want user", body.Role)
		}
	})

	t.Run("allowlist promotes listed", func(t *testing.T) {
		eng := gin.New()
		RegisterRoutes(eng.Group("/"), Deps{Auth: stub, Admins: NewAdminSet("alice@example.com")})
		code, body, _ := postAuth(t, eng, map[string]string{"user": currentUser, "password": "right"},
			map[string]string{"X-Periscope-User": "alice@example.com"})
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if body.Role != "admin" {
			t.Errorf("listed role = %q, want admin", body.Role)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		eng := gin.New()
		RegisterRoutes(eng.Group("/"), Deps{Auth: stub})
		code, _, _ := postAuth(t, eng, map[string]string{"user": currentUser, "password": "wrong"}, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
	})

	t.Run("user mismatch rejected", func(t *testing.T) {
		eng := gin.New()
		RegisterRoutes(eng.Group("/"), Deps{Auth: stub})
		code, _, _ := postAuth(t, eng, map[string]string{"user": currentUser + "-imposter", "password": "right"}, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
	})

	t.Run("missing user", func(t *testing.T) {
		eng := gin.New()
		RegisterRoutes(eng.Group("/"), Deps{Auth: stub})
		code, _, _ := postAuth(t, eng, map[string]string{"password": "right"}, nil)
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", code)
		}
	})
}

// TestAuthDelegated covers the custom-AuthURL branch:
//   - The agent forwards {user,password} verbatim.
//   - The returned {user,role} is trusted (the OS path's allowlist is
//     ignored).
//   - A non-200 from the endpoint surfaces as 401.
//   - A malformed or unknown role is normalized to "user".
//   - The agent's own OS user is NOT consulted on this path — an
//     unrelated username can authenticate as long as the endpoint
//     accepts it.
func TestAuthDelegated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type call struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	var lastCall call
	var lastPeriscopeUser string
	var status = http.StatusOK
	var respBody = `{"user":"app-bob","role":"admin"}`
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPeriscopeUser = r.Header.Get("X-Periscope-User")
		_ = json.NewDecoder(r.Body).Decode(&lastCall)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	defer external.Close()

	mkEngine := func() *gin.Engine {
		eng := gin.New()
		RegisterRoutes(eng.Group("/"), Deps{
			AuthURL: external.URL,
			// Admins is intentionally non-empty to prove the delegated
			// path ignores it entirely.
			Admins: NewAdminSet("only-alice@example.com"),
		})
		return eng
	}

	t.Run("forwards user+password and trusts returned role", func(t *testing.T) {
		status = http.StatusOK
		respBody = `{"user":"app-bob","role":"admin"}`
		code, body, _ := postAuth(t, mkEngine(),
			map[string]string{"user": "app-bob", "password": "secret"},
			map[string]string{"X-Periscope-User": "carol@example.com"})
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if lastCall.User != "app-bob" || lastCall.Password != "secret" {
			t.Errorf("endpoint received %+v, want user=app-bob password=secret", lastCall)
		}
		if lastPeriscopeUser != "carol@example.com" {
			t.Errorf("X-Periscope-User passthrough = %q, want carol@example.com", lastPeriscopeUser)
		}
		if body.Role != "admin" || body.User != "app-bob" {
			t.Errorf("response = %+v, want {app-bob, admin}", body)
		}
	})

	t.Run("non-200 → 401", func(t *testing.T) {
		status = http.StatusForbidden
		respBody = `nope`
		code, _, _ := postAuth(t, mkEngine(),
			map[string]string{"user": "app-bob", "password": "wrong"}, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
	})

	t.Run("unknown role normalized to user", func(t *testing.T) {
		status = http.StatusOK
		respBody = `{"user":"app-bob","role":"emperor"}`
		_, body, _ := postAuth(t, mkEngine(),
			map[string]string{"user": "app-bob", "password": "secret"}, nil)
		if body.Role != "user" {
			t.Errorf("role = %q, want user", body.Role)
		}
	})

	t.Run("malformed response → 401", func(t *testing.T) {
		status = http.StatusOK
		respBody = `not json`
		code, _, errBody := postAuth(t, mkEngine(),
			map[string]string{"user": "app-bob", "password": "secret"}, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
		if !strings.Contains(errBody, "malformed") {
			t.Errorf("error body = %q, want it to mention malformed", errBody)
		}
	})

	t.Run("OS user not consulted on delegated path", func(t *testing.T) {
		status = http.StatusOK
		respBody = `{"user":"some-app-user","role":"user"}`
		_, body, _ := postAuth(t, mkEngine(),
			map[string]string{"user": "some-app-user", "password": "secret"}, nil)
		if body.User != "some-app-user" {
			t.Errorf("user = %q, want some-app-user", body.User)
		}
	})
}
