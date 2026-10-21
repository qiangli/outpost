package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeGitFiller stands in for the `git credential fill` exec so the git
// app tests never touch a real credential store. It counts calls so
// tests can assert the in-memory credential cache is honored.
type fakeGitFiller struct {
	mu    sync.Mutex
	calls int
	user  string
	pass  string
}

func (f *fakeGitFiller) fill(context.Context, string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.user, f.pass, nil
}

func (f *fakeGitFiller) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newGitTestEnv registers a git app named "code" pointing at an
// httptest upstream with an injected fake credential filler, fronted by
// the standard /app/:name/*p test router.
func newGitTestEnv(t *testing.T, upstreamURL string, allow []string, user, pass string) (*AppRegistry, *httptest.Server, *fakeGitFiller) {
	t.Helper()
	target, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	reg := NewAppRegistry()
	filler := &fakeGitFiller{user: user, pass: pass}
	reg.gitCredentialFill = filler.fill
	if err := reg.registerGit("code", target, allow, AppMeta{}); err != nil {
		t.Fatalf("registerGit: %v", err)
	}
	front := httptest.NewServer(newTestRouter(reg))
	t.Cleanup(front.Close)
	return reg, front, filler
}

// TestGitAppForwardsAllowlistedUploadPackWithHostCredential pins the
// clone/fetch happy path: the ref advertisement (GET info/refs?service=
// git-upload-pack) and the fetch (POST git-upload-pack) are forwarded
// to the https upstream with the HOST credential (obtained via the
// injectable fill func) as Basic auth — while the caller's own
// Authorization/Cookie headers are stripped, never relayed. The second
// request reuses the cached credential (one fill call).
func TestGitAppForwardsAllowlistedUploadPackWithHostCredential(t *testing.T) {
	const (
		ciUser = "octocat"
		ciPass = "host-held-secret-token"
	)
	type upstreamSeen struct {
		method, path, query, auth, proto, body string
	}
	var mu sync.Mutex
	var got upstreamSeen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		mu.Lock()
		got = upstreamSeen{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			auth:   r.Header.Get("Authorization"),
			proto:  r.Header.Get("Git-Protocol"),
			body:   string(body),
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "001e# service=git-upload-pack")
	}))
	t.Cleanup(upstream.Close)

	_, front, filler := newGitTestEnv(t, upstream.URL, []string{"OctoCat/Hello", "acme/*"}, ciUser, ciPass)
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(ciUser+":"+ciPass))

	// Clone: ref advertisement.
	req, _ := http.NewRequest("GET", front.URL+"/app/code/octocat/hello.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Git-Protocol", "version=2")
	// Caller-supplied credentials must be stripped, never relayed.
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("attacker:hunter2")))
	req.Header.Set("Cookie", "session=stolen")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("info/refs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info/refs status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "001e# service=git-upload-pack" {
		t.Errorf("info/refs body = %q", body)
	}
	mu.Lock()
	saw := got
	mu.Unlock()
	if saw.path != "/octocat/hello.git/info/refs" {
		t.Errorf("upstream path = %q", saw.path)
	}
	if saw.query != "service=git-upload-pack" {
		t.Errorf("upstream query = %q", saw.query)
	}
	if saw.auth != wantAuth {
		t.Errorf("upstream Authorization = %q, want host credential %q injected", saw.auth, wantAuth)
	}
	if saw.proto != "version=2" {
		t.Errorf("upstream Git-Protocol = %q, want version=2", saw.proto)
	}

	// Fetch: upload-pack POST.
	preq, _ := http.NewRequest("POST", front.URL+"/app/code/octocat/hello/git-upload-pack", strings.NewReader("0032want 0f1e"))
	preq.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	preq.Header.Set("Git-Protocol", "version=2")
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("git-upload-pack: %v", err)
	}
	_, _ = io.Copy(io.Discard, presp.Body)
	_ = presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("git-upload-pack status = %d, want 200", presp.StatusCode)
	}
	mu.Lock()
	saw = got
	mu.Unlock()
	if saw.method != http.MethodPost || saw.path != "/octocat/hello/git-upload-pack" {
		t.Errorf("upstream saw %s %q", saw.method, saw.path)
	}
	if saw.body != "0032want 0f1e" {
		t.Errorf("upstream body = %q", saw.body)
	}
	if saw.auth != wantAuth {
		t.Errorf("upstream Authorization on POST = %q, want host credential", saw.auth)
	}
	// Two proxied requests, one credential fill: the pair is cached.
	if n := filler.callCount(); n != 1 {
		t.Errorf("credential fill called %d times, want 1 (cached)", n)
	}
}

// TestGitAppRefusesReceivePackAndUnlistedRepo pins the default-deny
// shape of the git app: push (git-receive-pack) in both of its smart
// HTTP forms, unlisted owners/repos, non-git endpoints, missing service
// params, wrong methods, traversal-shaped segments, and an app with an
// empty allowlist are ALL refused with 403 without ever reaching the
// upstream. The "owner/*" wildcard entry still admits a fetch POST.
func TestGitAppRefusesReceivePackAndUnlistedRepo(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	reg, front, _ := newGitTestEnv(t, upstream.URL, []string{"octocat/hello", "acme/*"}, "u", "p")
	// A second app with an empty allowlist must refuse everything.
	target, _ := url.Parse(upstream.URL)
	if err := reg.registerGit("sealed", target, nil, AppMeta{}); err != nil {
		t.Fatalf("registerGit sealed: %v", err)
	}

	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"receive-pack info/refs refused", "GET", "/app/code/octocat/hello.git/info/refs?service=git-receive-pack", http.StatusForbidden},
		{"receive-pack POST refused", "POST", "/app/code/octocat/hello/git-receive-pack", http.StatusForbidden},
		{"receive-pack POST .git refused", "POST", "/app/code/octocat/hello.git/git-receive-pack", http.StatusForbidden},
		{"unlisted owner refused", "GET", "/app/code/evilcorp/repo/info/refs?service=git-upload-pack", http.StatusForbidden},
		{"unlisted repo under listed owner refused", "GET", "/app/code/octocat/other/info/refs?service=git-upload-pack", http.StatusForbidden},
		{"non-git endpoint refused", "GET", "/app/code/octocat/hello/HEAD", http.StatusForbidden},
		{"missing service refused", "GET", "/app/code/octocat/hello/info/refs", http.StatusForbidden},
		{"wrong method refused", "PUT", "/app/code/octocat/hello/git-upload-pack", http.StatusForbidden},
		{"traversal-shaped segment refused", "GET", "/app/code/%2e%2e/octocat/hello/info/refs?service=git-upload-pack", http.StatusForbidden},
		{"empty allowlist refuses all", "GET", "/app/sealed/octocat/hello/info/refs?service=git-upload-pack", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, front.URL+tc.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s: status = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
			}
		})
	}
	mu.Lock()
	if hits != 0 {
		mu.Unlock()
		t.Errorf("upstream hit %d times by refused requests, want 0", hits)
	} else {
		mu.Unlock()
	}

	// The wildcard entry still admits a fetch POST.
	preq, _ := http.NewRequest("POST", front.URL+"/app/code/acme/widgets/git-upload-pack", nil)
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("wildcard fetch: %v", err)
	}
	_, _ = io.Copy(io.Discard, presp.Body)
	_ = presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("wildcard fetch status = %d, want 200", presp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("upstream hits = %d after wildcard fetch, want 1", hits)
	}
}

// TestGitAppNeverLeaksCredential points the git app at a hostile
// upstream that echoes the Authorization header it received back in
// response headers, 200 bodies, a 401 error body, and a redirect
// Location. None of those surfaces — nor the agent's logs — may carry
// the host credential (raw password, user:password, or its base64
// Basic-auth encoding) back to the caller.
func TestGitAppNeverLeaksCredential(t *testing.T) {
	const (
		ciUser = "octocat"
		ciPass = "top-secret-host-credential"
	)
	basic := base64.StdEncoding.EncodeToString([]byte(ciUser + ":" + ciPass))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(r.URL.Path, "/leak"):
			w.Header().Set("Echo-Auth", auth)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "upstream saw "+auth)
		case strings.Contains(r.URL.Path, "/denied"):
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "credentials rejected "+auth)
		case strings.Contains(r.URL.Path, "/redir"):
			w.Header().Set("Location", "https://example.com/x#"+auth)
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(upstream.Close)

	_, front, _ := newGitTestEnv(t, upstream.URL, []string{"octocat/*"}, ciUser, ciPass)

	// Capture everything the agent logs while the requests run.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	assertClean := func(t *testing.T, where, s string) {
		t.Helper()
		for _, needle := range []string{ciPass, ciUser + ":" + ciPass, basic} {
			if strings.Contains(s, needle) {
				t.Errorf("%s contains the host credential (%q leaked)", where, needle)
			}
		}
	}
	headersToString := func(h http.Header) string {
		var b bytes.Buffer
		_ = h.Write(&b)
		return b.String()
	}

	t.Run("200 body and headers scrubbed", func(t *testing.T) {
		resp, err := http.Get(front.URL + "/app/code/octocat/leak.git/info/refs?service=git-upload-pack")
		if err != nil {
			t.Fatalf("get leak: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assertClean(t, "200 body", string(body))
		assertClean(t, "200 headers", headersToString(resp.Header))
	})

	t.Run("error body replaced", func(t *testing.T) {
		req, _ := http.NewRequest("POST", front.URL+"/app/code/octocat/denied.git/git-upload-pack", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post denied: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assertClean(t, "error body", string(body))
		assertClean(t, "error headers", headersToString(resp.Header))
	})

	t.Run("redirect location scrubbed", func(t *testing.T) {
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Get(front.URL + "/app/code/octocat/redir.git/info/refs?service=git-upload-pack")
		if err != nil {
			t.Fatalf("get redir: %v", err)
		}
		defer resp.Body.Close()
		assertClean(t, "redirect Location", resp.Header.Get("Location"))
		assertClean(t, "redirect headers", headersToString(resp.Header))
	})

	assertClean(t, "logs", logBuf.String())
}
