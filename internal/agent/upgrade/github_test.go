package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub stands in for api.github.com + the releases.download host.
// It serves the latest-release JSON, the tag-ref deref chain, and the
// sha256 sidecar, all off the same httptest server so GitHubSource can
// be pointed at it wholesale.
func fakeGitHub(t *testing.T, tag, goos, goarch, commit string, annotated bool) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	// TLS server: the resolved download URL must pass Envelope.Validate's
	// https-only check, so the asset URLs we hand back must be https too.
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	binName := fmt.Sprintf("outpost-%s-%s-%s", tag, goos, goarch)
	if goos == "windows" {
		binName += ".exe"
	}
	shaName := fmt.Sprintf("outpost-%s-%s-%s.sha256", tag, goos, goarch)
	sum := strings.Repeat("a", 64)

	mux.HandleFunc("/repos/qiangli/outpost/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[
			{"name":%q,"browser_download_url":%q},
			{"name":%q,"browser_download_url":%q}
		]}`, tag,
			binName, srv.URL+"/dl/"+binName,
			shaName, srv.URL+"/dl/"+shaName)
	})

	mux.HandleFunc("/repos/qiangli/outpost/git/ref/tags/"+tag, func(w http.ResponseWriter, _ *http.Request) {
		if annotated {
			// Points at a tag object that must be dereferenced.
			fmt.Fprintf(w, `{"object":{"type":"tag","sha":"tagobjsha"}}`)
			return
		}
		fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, commit)
	})
	mux.HandleFunc("/repos/qiangli/outpost/git/tags/tagobjsha", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"object":{"sha":%q}}`, commit)
	})

	mux.HandleFunc("/dl/"+shaName, func(w http.ResponseWriter, _ *http.Request) {
		// shasum -a 256 format: "<hex>  <filename>"
		fmt.Fprintf(w, "%s  %s\n", sum, binName)
	})

	return srv, sum
}

func TestGitHubResolveLightweightTag(t *testing.T) {
	srv, sum := fakeGitHub(t, "v0.8.0", "darwin", "arm64", "abc1234def5678", false)
	env, err := GitHubSource{Platform: "darwin_arm64", apiBase: srv.URL, HTTPClient: srv.Client()}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if env.ReleaseID != "v0.8.0" {
		t.Errorf("ReleaseID = %q, want v0.8.0", env.ReleaseID)
	}
	if env.SHA256 != sum {
		t.Errorf("SHA256 = %q, want %q", env.SHA256, sum)
	}
	if env.Commit != "abc1234" { // shortened to 7
		t.Errorf("Commit = %q, want abc1234", env.Commit)
	}
	if !strings.HasSuffix(env.URL, "/dl/outpost-v0.8.0-darwin-arm64") {
		t.Errorf("URL = %q, unexpected", env.URL)
	}
	if err := env.Validate(); err != nil {
		t.Errorf("resolved envelope failed Validate: %v", err)
	}
}

func TestGitHubResolveAnnotatedTagDeref(t *testing.T) {
	srv, _ := fakeGitHub(t, "v1.0.0", "linux", "amd64", "deadbeefcafe", true)
	env, err := GitHubSource{Platform: "linux_amd64", apiBase: srv.URL, HTTPClient: srv.Client()}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if env.Commit != "deadbee" {
		t.Errorf("Commit = %q, want deadbee (deref'd through annotated tag)", env.Commit)
	}
}

func TestGitHubResolveWindowsAsset(t *testing.T) {
	srv, _ := fakeGitHub(t, "v0.8.0", "windows", "amd64", "0011223344", false)
	env, err := GitHubSource{Platform: "windows_amd64", apiBase: srv.URL, HTTPClient: srv.Client()}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasSuffix(env.URL, ".exe") {
		t.Errorf("windows URL = %q, want .exe suffix", env.URL)
	}
}

func TestGitHubResolveMissingPlatform(t *testing.T) {
	// Server only publishes darwin/arm64; ask for linux/arm64.
	srv, _ := fakeGitHub(t, "v0.8.0", "darwin", "arm64", "abc1234", false)
	_, err := GitHubSource{Platform: "linux_arm64", apiBase: srv.URL, HTTPClient: srv.Client()}.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for unpublished platform, got nil")
	}
	if !strings.Contains(err.Error(), "no asset") {
		t.Errorf("error = %v, want 'no asset'", err)
	}
}

func TestGitHubResolveBadPlatform(t *testing.T) {
	_, err := GitHubSource{Platform: "garbage"}.Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "malformed platform") {
		t.Errorf("error = %v, want malformed platform", err)
	}
}

func TestParseSHA256Sidecar(t *testing.T) {
	sum := strings.Repeat("b", 64)
	cases := []struct {
		name    string
		body    string
		bin     string
		want    string
		wantErr bool
	}{
		{"plain", sum + "  outpost-v1-linux-amd64\n", "outpost-v1-linux-amd64", sum, false},
		{"binary-marker", sum + " *outpost-v1-linux-amd64\n", "outpost-v1-linux-amd64", sum, false},
		{"path-qualified", sum + "  dist/outpost-v1-linux-amd64\n", "outpost-v1-linux-amd64", sum, false},
		{"multi-line", "deadbeef  other\n" + sum + "  target\n", "target", sum, false},
		{"missing", sum + "  other\n", "target", "", true},
		{"short-digest", "abcd  target\n", "target", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSHA256Sidecar(tc.body, tc.bin)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGitHubRateLimited(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/repos/qiangli/outpost/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := GitHubSource{Platform: "darwin_arm64", apiBase: srv.URL}.Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %v, want rate-limited hint", err)
	}
}
