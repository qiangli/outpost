package vknode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFetchKubeconfig_SuccessfulDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != FetchEndpointPath || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "wrong", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer my-token" {
			t.Errorf("Authorization: got %q want %q", got, "Bearer my-token")
		}
		var req FetchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.NodeName != "home-mini" {
			t.Errorf("NodeName: got %q want home-mini", req.NodeName)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"api_url": "https://cloudbox.example/api/cluster/agent",
			"token": "fresh-sa-token",
			"ca_data": "`+base64.StdEncoding.EncodeToString([]byte("fake-ca"))+`",
			"node_name": "home-mini"
		}`)
	}))
	defer srv.Close()

	got, err := FetchKubeconfig(context.Background(), srv.URL, "my-token", "home-mini")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIURL != "https://cloudbox.example/api/cluster/agent" {
		t.Errorf("APIURL = %q", got.APIURL)
	}
	if got.Token != "fresh-sa-token" {
		t.Errorf("Token = %q", got.Token)
	}
	if string(got.CA) != "fake-ca" {
		t.Errorf("CA decoded = %q", string(got.CA))
	}
}

func TestFetchKubeconfig_Returns503AsFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"cluster mode is not enabled"}`)
	}))
	defer srv.Close()

	_, err := FetchKubeconfig(context.Background(), srv.URL, "my-token", "home-mini")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsClusterDisabled(err) {
		t.Errorf("IsClusterDisabled = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "cluster mode is not enabled") {
		t.Errorf("error message lost: %v", err)
	}
}

func TestFetchKubeconfig_Returns403ForBadHostOwnership(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"no host with that name"}`)
	}))
	defer srv.Close()

	_, err := FetchKubeconfig(context.Background(), srv.URL, "my-token", "ghost")
	if err == nil {
		t.Fatal("expected error")
	}
	if IsClusterDisabled(err) {
		t.Error("403 is not cluster-disabled")
	}
	var fe *FetchError
	if !asFetchError(err, &fe) || fe.Status != http.StatusForbidden {
		t.Errorf("want FetchError 403, got %T %v", err, err)
	}
}

func TestFetchKubeconfig_RejectsEmptyInputs(t *testing.T) {
	for _, tc := range []struct{ base, tok, name string }{
		{"", "tok", "n"},
		{"  ", "tok", "n"},
		{"http://x", "", "n"},
		{"http://x", "tok", ""},
	} {
		if _, err := FetchKubeconfig(context.Background(), tc.base, tc.tok, tc.name); err == nil {
			t.Errorf("expected error for empty input (base=%q tok=%q name=%q)", tc.base, tc.tok, tc.name)
		}
	}
}

func TestTokenExpiry_ParsesUnverifiedJWT(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour).Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp, 10) + `,"sub":"sa"}`))
	sig := base64.RawURLEncoding.EncodeToString([]byte("fake-signature"))
	jwt := header + "." + payload + "." + sig

	got := TokenExpiry(jwt)
	if got.Unix() != exp {
		t.Errorf("exp: got %d want %d", got.Unix(), exp)
	}
}

func TestTokenExpiry_NonJWTReturnsZero(t *testing.T) {
	cases := []string{
		"opaque-token",
		"",
		"abc.def", // only two segments
		"abc.def.ghi.jkl",
	}
	for _, c := range cases {
		if !TokenExpiry(c).IsZero() {
			t.Errorf("TokenExpiry(%q) should be zero", c)
		}
	}
}

func TestNextRefreshDelay(t *testing.T) {
	now := time.Now()
	// Empty token: minimum interval
	if d := nextRefreshDelay("", now); d != minRefreshInterval {
		t.Errorf("empty token: got %v want %v", d, minRefreshInterval)
	}
	// Token with no exp: minimum interval
	noExp := makeJWT(t, map[string]any{"sub": "sa"})
	if d := nextRefreshDelay(noExp, now); d != minRefreshInterval {
		t.Errorf("no-exp token: got %v want %v", d, minRefreshInterval)
	}
	// Token with 24h exp: ~12h wait
	tok24h := makeJWT(t, map[string]any{"exp": now.Add(24 * time.Hour).Unix()})
	d := nextRefreshDelay(tok24h, now)
	want := 12 * time.Hour
	if abs(d-want) > time.Minute {
		t.Errorf("24h token: got %v want ~%v", d, want)
	}
	// Token with 5min remaining: minRefreshInterval floor kicks in
	tok5m := makeJWT(t, map[string]any{"exp": now.Add(5 * time.Minute).Unix()})
	if d := nextRefreshDelay(tok5m, now); d != minRefreshInterval {
		t.Errorf("near-expiry token: got %v want %v", d, minRefreshInterval)
	}
}

func TestWriteTokenFile_AtomicAndMode(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "vktok")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "tok")

	if err := WriteTokenFile(path, "hello"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode: got %o want 0600", info.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Errorf("contents: %q", got)
	}
	// Second write should atomically replace.
	if err := WriteTokenFile(path, "world"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "world" {
		t.Errorf("after rewrite: %q", got)
	}
	// No leftover .tmp file
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp not cleaned up: err=%v", err)
	}
}

// helpers --------------------------------------------------------------

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fake-sig"))
	return header + "." + payload + "." + sig
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func asFetchError(err error, target **FetchError) bool {
	// errors.As without importing errors in this test file path.
	if fe, ok := err.(*FetchError); ok {
		*target = fe
		return true
	}
	return false
}
