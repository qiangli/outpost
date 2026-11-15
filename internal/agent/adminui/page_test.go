package adminui

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPageControlAuthAndOrigin(t *testing.T) {
	s := bearerServer(t)
	defer s.pages.Close()
	for _, tc := range []struct {
		name, remote, token string
		want                int
	}{
		{"local bearer", "127.0.0.1:5000", testMCPToken, 200},
		{"missing", "127.0.0.1:5000", "", 401},
		{"wrong", "127.0.0.1:5000", "invalid", 401},
		{"remote bearer", "192.0.2.1:5000", testMCPToken, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := bearerReq(s, "GET", "/api/page/pages", tc.remote, "Bearer "+tc.token)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
	req := httptest.NewRequest("GET", "http://outpost.test/api/page/pages", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Authorization", "Bearer "+testMCPToken)
	req.Header.Set("Origin", "http://attacker.test")
	req.Header.Set("X-Forwarded-Host", "attacker.test")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("untrusted origin admitted: %d", w.Code)
	}
	req.Header.Set("Origin", "http://outpost.test")
	req.Header.Set("X-Forwarded-Prefix", "/host/")
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("same-host forwarded page rejected: %d", w.Code)
	}
}

func TestPageControlAllowlistExcludesPrivilegedControls(t *testing.T) {
	for _, action := range []string{"outpost.navigate", "outpost.shape.open", "outpost.shape-only", "outpost.networking.get", "outpost.networking.draft", "outpost.networking.submit"} {
		if !allowedPageAction(action) {
			t.Fatal(action)
		}
	}
	for _, action := range []string{"outpost.credentials.reveal", "outpost.credentials.rotate", "outpost.login", "outpost.admin-users.set", "eval", "click", ""} {
		if allowedPageAction(action) {
			t.Fatalf("privileged action allowed: %s", action)
		}
	}
}

func TestPageScriptsRemainRelativeBehindPrefix(t *testing.T) {
	s := bearerServer(t)
	defer s.pages.Close()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/host/")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	for _, want := range []string{`<base href="/host/">`, `src="page-control.js"`, `src="page-ui.js"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s", want)
		}
	}
	for _, path := range []string{"/page-control.js", "/page-ui.js"} {
		w = httptest.NewRecorder()
		s.engine.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestPageCookieSocketAcceptsAuthorizedBearerAndClosesOnLogout(t *testing.T) {
	s := bearerServer(t)
	defer s.pages.Close()
	server := httptest.NewServer(s.engine)
	defer server.Close()
	user, err := hostauth.CurrentUser()
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := s.sessions.Mint(user)
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Origin": []string{server.URL}, "Cookie": []string{cookieName + "=" + cookie}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/page/events", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err = ws.WriteJSON(map[string]any{"actions": []map[string]string{{"name": "outpost.networking.submit"}}, "label": "test page"}); err != nil {
		t.Fatal(err)
	}
	var pageID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w := bearerReq(s, "GET", "/api/page/pages", "127.0.0.1:5000", "Bearer "+testMCPToken)
		var body struct {
			Pages []struct {
				ID      string `json:"id"`
				Actions []any  `json:"actions"`
			} `json:"pages"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Pages) == 1 && len(body.Pages[0].Actions) == 1 {
			pageID = body.Pages[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pageID == "" {
		t.Fatal("page did not register")
	}
	req := httptest.NewRequest("POST", "/api/page/invoke", strings.NewReader(`{"page_id":"`+pageID+`","action":"outpost.networking.submit","input":null}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Authorization", "Bearer "+testMCPToken)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("invoke %d: %s", w.Code, w.Body.String())
	}
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	var message struct {
		Command struct {
			ID     string `json:"id"`
			Action string `json:"action"`
		} `json:"command"`
	}
	for message.Command.ID == "" {
		if err = ws.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
	}
	if message.Command.Action != "outpost.networking.submit" {
		t.Fatalf("unexpected %v", message)
	}
	// Logout fails the in-flight command, rather than replaying it into a new page.
	req = httptest.NewRequest("POST", "/api/logout", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("logout %d", w.Code)
	}
	if _, _, err = ws.ReadMessage(); err == nil {
		t.Fatal("socket survived logout")
	}
	w = bearerReq(s, "GET", "/api/page/result?id="+message.Command.ID, "127.0.0.1:5000", "Bearer "+testMCPToken)
	if !strings.Contains(w.Body.String(), `"status":"expired"`) {
		t.Fatalf("pending command not invalidated: %s", w.Body.String())
	}
}
