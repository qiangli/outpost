package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultRepo is the GitHub repository the direct-from-GitHub upgrade
// path resolves releases from when GitHubSource.Repo is empty.
const DefaultRepo = "qiangli/outpost"

const (
	githubAPIBase     = "https://api.github.com"
	githubAPIVersion  = "2022-11-28"
	githubUserAgent   = "outpost-upgrade"
	githubResolveTOut = 20 * time.Second
)

// GitHubSource resolves the latest published release of an outpost repo
// into an Envelope the Worker / CLI swap flow can apply WITHOUT a
// cloudbox in the loop.
//
// This is the fallback upgrade authority for the two cases the cloudbox
// push/pull path structurally can't serve:
//
//   - an UNPAIRED host — no access_token, so no /api/v1/fleet/target to
//     poll. `outpost upgrade` with no --from/--local resolves here.
//   - the `outpost upgrade --direct` operator escape hatch on a paired
//     host — deliberately bypassing fleet governance. An interactive
//     operator standing at the box already holds OS-level authority over
//     it, the same authority `outpost upgrade --from <url>` has always
//     assumed; --direct is just that with the URL resolved for them.
//
// It produces the SAME Envelope shape the cloudbox release webhook
// produces, so everything downstream is unchanged: StageFromURL verifies
// the sha256, Probe rejects a candidate whose self-reported commit
// doesn't match, RetainPrevious leaves a rollback copy, then the atomic
// swap. The only thing that shifts is the artifact owner — GitHub
// Releases over HTTPS instead of cloudbox.
//
// Deliberately NOT wired into an automatic background poller: a paired
// host's automatic upgrades stay governed by the fleet (so cloudbox
// keeps controlling canary→fleet rollout, update_mode, and min_from
// fences), and an unpaired host self-upgrading silently could surprise
// an operator who pinned a build on purpose. Direct resolution is an
// explicit, interactive action.
type GitHubSource struct {
	// Repo is "owner/name"; empty → DefaultRepo.
	Repo string
	// Platform is "<goos>_<goarch>" (e.g. "darwin_arm64"), matching the
	// release asset naming `outpost-<tag>-<goos>-<goarch>[.exe]` from
	// .github/workflows/release.yml.
	Platform string
	// HTTPClient for api.github.com calls + the sidecar download; nil →
	// http.DefaultClient.
	HTTPClient *http.Client
	// Token, when set, is sent as a Bearer to api.github.com to lift the
	// 60-req/hr unauthenticated rate limit. Optional — the public
	// releases API works unauthenticated for the handful of calls a
	// single interactive upgrade makes.
	Token string

	// apiBase overrides the api.github.com origin in tests. Empty →
	// githubAPIBase. Unexported on purpose: production callers never set
	// it.
	apiBase string
}

// ghRelease is the slice of the GitHub "latest release" response we use.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// assetURL returns the download URL of the asset named exactly `name`,
// or "" if the release doesn't carry it.
func (r ghRelease) assetURL(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// Resolve fetches the latest release for the configured platform and
// returns a ready-to-apply Envelope. Errors are phrased for an operator
// reading them straight off the terminal (no matching asset, rate
// limited, etc.).
func (g GitHubSource) Resolve(ctx context.Context) (Envelope, error) {
	goos, goarch, ok := splitPlatform(g.Platform)
	if !ok {
		return Envelope{}, fmt.Errorf("malformed platform %q (want <goos>_<goarch>, e.g. darwin_arm64)", g.Platform)
	}
	repo := strings.TrimSpace(g.Repo)
	if repo == "" {
		repo = DefaultRepo
	}

	rel, err := g.fetchLatest(ctx, repo)
	if err != nil {
		return Envelope{}, err
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return Envelope{}, errors.New("latest release has no tag_name")
	}

	// Asset naming mirrors .github/workflows/release.yml:
	//   outpost-<tag>-<goos>-<goarch>[.exe]      (the binary)
	//   outpost-<tag>-<goos>-<goarch>.sha256     (its sidecar)
	binName := fmt.Sprintf("outpost-%s-%s-%s", rel.TagName, goos, goarch)
	if goos == "windows" {
		binName += ".exe"
	}
	shaName := fmt.Sprintf("outpost-%s-%s-%s.sha256", rel.TagName, goos, goarch)

	binURL := rel.assetURL(binName)
	if binURL == "" {
		return Envelope{}, fmt.Errorf("release %s has no asset %q — platform %s may not be published for this release", rel.TagName, binName, g.Platform)
	}
	shaURL := rel.assetURL(shaName)
	if shaURL == "" {
		return Envelope{}, fmt.Errorf("release %s has no sha256 sidecar %q — refusing to upgrade without an integrity check", rel.TagName, shaName)
	}

	sum, err := g.fetchSHA256(ctx, shaURL, binName)
	if err != nil {
		return Envelope{}, fmt.Errorf("resolve sha256 for %s: %w", binName, err)
	}

	// The release binary is built from the tagged commit (release.yml is
	// tag-triggered and checks out that tag), so its BuildInfo.Commit ==
	// the tag's commit. Resolving it here lets Probe enforce an exact
	// match and lets Apply's same-commit guard no-op when already current.
	commit, err := g.resolveTagCommit(ctx, repo, rel.TagName)
	if err != nil {
		return Envelope{}, fmt.Errorf("resolve commit for tag %s: %w", rel.TagName, err)
	}

	env := Envelope{
		ReleaseID: rel.TagName,
		URL:       binURL,
		SHA256:    sum,
		Commit:    commit,
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

func (g GitHubSource) fetchLatest(ctx context.Context, repo string) (ghRelease, error) {
	var rel ghRelease
	url := g.base() + "/repos/" + repo + "/releases/latest"
	if err := g.getJSON(ctx, url, &rel); err != nil {
		return ghRelease{}, err
	}
	return rel, nil
}

// resolveTagCommit dereferences a tag ref to its commit sha (7-char),
// handling both lightweight tags (ref points straight at the commit)
// and annotated tags (ref points at a tag object that points at the
// commit) — the same two-hop dance release.yml does to read tag
// annotations.
func (g GitHubSource) resolveTagCommit(ctx context.Context, repo, tag string) (string, error) {
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := g.getJSON(ctx, g.base()+"/repos/"+repo+"/git/ref/tags/"+tag, &ref); err != nil {
		return "", err
	}
	sha := ref.Object.SHA
	if ref.Object.Type == "tag" {
		var tagObj struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := g.getJSON(ctx, g.base()+"/repos/"+repo+"/git/tags/"+sha, &tagObj); err != nil {
			return "", err
		}
		sha = tagObj.Object.SHA
	}
	if strings.TrimSpace(sha) == "" {
		return "", errors.New("empty commit sha")
	}
	return shortCommit(sha), nil
}

// fetchSHA256 downloads the `shasum -a 256`-format sidecar and returns
// the hex digest for the line whose filename matches binName. The
// sidecar is served from github.com/.../releases/download (not the API
// host), so it doesn't count against the API rate limit.
func (g GitHubSource) fetchSHA256(ctx context.Context, shaURL, binName string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, githubResolveTOut)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, shaURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", githubUserAgent)
	resp, err := g.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download sidecar: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	return parseSHA256Sidecar(string(body), binName)
}

// parseSHA256Sidecar extracts the hex digest for binName from
// `shasum -a 256` output ("<hex>  <filename>", possibly multiple
// lines). Matches on the filename's basename so a sidecar that records
// a path-qualified name still resolves.
func parseSHA256Sidecar(body, binName string) (string, error) {
	for line := range strings.SplitSeq(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hex, name := fields[0], fields[len(fields)-1]
		name = strings.TrimPrefix(name, "*") // shasum binary-mode marker
		if base(name) == binName {
			if len(hex) != 64 {
				return "", fmt.Errorf("sidecar digest for %s is not 64 hex chars", binName)
			}
			return hex, nil
		}
	}
	return "", fmt.Errorf("sidecar has no entry for %s", binName)
}

func (g GitHubSource) getJSON(ctx context.Context, url string, out any) error {
	cctx, cancel := context.WithTimeout(ctx, githubResolveTOut)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	case http.StatusForbidden, http.StatusTooManyRequests:
		// Almost always the unauthenticated 60-req/hr limit. Point the
		// operator at the token escape hatch rather than a raw 403.
		return fmt.Errorf("GitHub API %d (rate limited?) — set GITHUB_TOKEN to raise the limit", resp.StatusCode)
	case http.StatusNotFound:
		return fmt.Errorf("GitHub API 404 for %s — no such repo, or no published release yet", url)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("GitHub API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func (g GitHubSource) client() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return http.DefaultClient
}

func (g GitHubSource) base() string {
	if g.apiBase != "" {
		return g.apiBase
	}
	return githubAPIBase
}

// splitPlatform splits "<goos>_<goarch>" — goos never contains an
// underscore, so SplitN on the first "_" is unambiguous.
func splitPlatform(p string) (goos, goarch string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(p), "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// base returns the final path element of a (possibly path-qualified)
// filename, using both separators so a Windows-authored sidecar still
// resolves on a unix host.
func base(name string) string {
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		return name[i+1:]
	}
	return name
}
