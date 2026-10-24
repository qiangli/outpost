package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
)

// fakeGitHubReleases serves a releases LIST (plus the legacy /latest
// endpoint, tag derefs, and sha sidecars) so tests can prove --direct
// picks the newest strictly-newer release by published date instead of
// trusting GitHub's "latest" (which ignores prereleases).
func fakeGitHubReleases(t *testing.T, releases []ghRelease, commits map[string]string) (*httptest.Server, *http.Client) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	var list []string
	for _, rel := range releases {
		tag := rel.TagName
		assets := ""
		for i, a := range rel.Assets {
			if i > 0 {
				assets += ","
			}
			// Rewrite asset URLs to this fake server.
			url := srv.URL + "/dl/" + a.Name
			rel.Assets[i].BrowserDownloadURL = url
			assets += fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, a.Name, url)
		}
		list = append(list, fmt.Sprintf(`{"tag_name":%q,"published_at":%q,"assets":[%s]}`,
			tag, rel.PublishedAt, assets))
		_ = tag
	}
	mux.HandleFunc("/repos/qiangli/outpost/releases", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[%s]`, strings.Join(list, ","))
	})
	// The buggy authority: "latest" still points at the old stable.
	mux.HandleFunc("/repos/qiangli/outpost/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v0.32.0","published_at":"2026-10-08T00:00:00Z","assets":[]}`)
	})
	mux.HandleFunc("/repos/qiangli/outpost/git/ref/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/repos/qiangli/outpost/git/ref/tags/")
		fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, commits[tag])
	})
	sum := strings.Repeat("b", 64)
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		if strings.HasSuffix(name, ".sha256") {
			// shasum sidecar: the digest line names the BINARY.
			fmt.Fprintf(w, "%s  %s\n", sum, strings.TrimSuffix(name, ".sha256"))
			return
		}
		fmt.Fprintf(w, "binary-bytes-for-%s", name)
	})
	return srv, srv.Client()
}

// releaseWithAssets builds a ghRelease carrying this platform's binary +
// sidecar asset names (URLs are rewritten to the fake server on serve).
func releaseWithAssets(tag, publishedAt, goos, goarch string) ghRelease {
	rel := ghRelease{TagName: tag, PublishedAt: publishedAt}
	base := strings.TrimSuffix(tag, "-dev")
	bin := fmt.Sprintf("outpost-%s-%s-%s", base, goos, goarch)
	for _, name := range []string{bin, bin + ".sha256"} {
		rel.Assets = append(rel.Assets, struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{Name: name})
	}
	return rel
}

// TestDirectPicksNewestNewerRelease is the 2026-10-24 downgrade incident regression: a host on
// v1.0.0-rc.10-dev must resolve --direct to v1.0.0-rc.12-dev (published
// 2026-10-23), never to GitHub "latest" v0.32.0 (2026-10-08).
func TestDirectPicksNewestNewerRelease(t *testing.T) {
	releases := []ghRelease{
		releaseWithAssets("v0.32.0", "2026-10-08T00:00:00Z", "darwin", "arm64"),
		releaseWithAssets("v1.0.0-rc.12-dev", "2026-10-23T00:00:00Z", "darwin", "arm64"),
	}
	commits := map[string]string{
		"v0.32.0":          "aaaaaaa1111111",
		"v1.0.0-rc.12-dev": "bbbbbbb2222222",
	}
	srv, client := fakeGitHubReleases(t, releases, commits)
	env, err := GitHubSource{
		Repo:       "qiangli/outpost",
		Platform:   "darwin_arm64",
		apiBase:    srv.URL,
		HTTPClient: client,
		Current:    agent.BuildInfo{Version: "v1.0.0-rc.10-dev", VCSTime: "2026-10-20T00:00:00Z"},
	}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if env.ReleaseID != "v1.0.0-rc.12-dev" {
		t.Errorf("ReleaseID = %q, want v1.0.0-rc.12-dev (must never be v0.32.0)", env.ReleaseID)
	}
	if env.BuildTime == "" {
		t.Error("BuildTime is empty, want the chosen release's published_at as candidate build time")
	}
	if err := env.Validate(); err != nil {
		t.Errorf("resolved envelope failed Validate: %v", err)
	}
}

// TestDirectNoNewerReleaseRefuses: when every published release is older
// than the running build, Resolve must fail loudly, not hand back a
// downgrade.
func TestDirectNoNewerReleaseRefuses(t *testing.T) {
	releases := []ghRelease{
		releaseWithAssets("v0.32.0", "2026-10-08T00:00:00Z", "darwin", "arm64"),
	}
	commits := map[string]string{"v0.32.0": "aaaaaaa1111111"}
	srv, client := fakeGitHubReleases(t, releases, commits)
	_, err := GitHubSource{
		Repo:       "qiangli/outpost",
		Platform:   "darwin_arm64",
		apiBase:    srv.URL,
		HTTPClient: client,
		Current:    agent.BuildInfo{Version: "v1.0.0-rc.10-dev", VCSTime: "2026-10-20T00:00:00Z"},
	}.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected refusal when no release is newer than the running build, got nil")
	}
}
