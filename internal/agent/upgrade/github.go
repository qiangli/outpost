package upgrade

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/yoke/pkg/gomod"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/qiangli/outpost/internal/agent"
)

// DefaultRepo is the GitHub repository the direct-from-GitHub upgrade
// path resolves releases from when GitHubSource.Repo is empty.
const DefaultRepo = "qiangli/bashy"

const (
	githubAPIBase     = "https://api.github.com"
	githubAPIVersion  = "2022-11-28"
	githubUserAgent   = "outpost-upgrade"
	githubResolveTOut = 20 * time.Second
)

// GitHubSource resolves the latest published product release
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
	// raw asset naming `outpost-<base-tag>-<goos>-<goarch>[.exe]` from
	// bashy's release workflow.
	Platform string
	// HTTPClient for api.github.com calls + the sidecar download; nil →
	// http.DefaultClient.
	HTTPClient *http.Client
	// Token, when set, is sent as a Bearer to api.github.com to lift the
	// 60-req/hr unauthenticated rate limit. Optional — the public
	// releases API works unauthenticated for the handful of calls a
	// single interactive upgrade makes.
	Token string

	// Current is the running build. When Current.Version is valid
	// semver, Resolve ignores GitHub's "latest" pointer (which skips
	// prereleases — the 2026-10-24 downgrade incident: "latest" v0.32.0
	// resolved over a newer rc.10-dev) and instead picks the NEWEST release by
	// published date among releases strictly newer than the running
	// build. Empty (unknown version) keeps the legacy "latest"
	// behavior. The CLI always sets this from the daemon's status.
	Current agent.BuildInfo

	// apiBase overrides the api.github.com origin in tests. Empty →
	// githubAPIBase. Unexported on purpose: production callers never set
	// it.
	apiBase string
}

// ghRelease is the slice of the GitHub release response we use, for
// both the "latest" endpoint and the releases list.
type ghRelease struct {
	TagName     string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
	Draft       bool   `json:"draft"`
	Assets      []struct {
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

	rel, err := g.pickRelease(ctx, repo, goos, goarch)
	if err != nil {
		return Envelope{}, err
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return Envelope{}, errors.New("latest release has no tag_name")
	}

	// Asset naming mirrors bashy's release workflow. The raw executable keeps
	// the base version when candidate bytes move from -dev to stable.
	binName, shaName, legacySHA := releaseAssetNames(rel.TagName, goos, goarch)
	if rel.assetURL(shaName) == "" {
		shaName = legacySHA // legacy Windows sidecar
	}

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

	// A product tag belongs to bashy; the outpost source identity is its pinned
	// sibling, never the tag's bashy commit. Explicit legacy repos retain tag lookup.
	var commit string
	if repo == DefaultRepo {
		commit, err = g.resolveOutpostPin(ctx, repo, rel.TagName)
	} else {
		commit, err = g.resolveTagCommit(ctx, repo, rel.TagName)
	}
	if err != nil {
		return Envelope{}, fmt.Errorf("resolve commit for tag %s: %w", rel.TagName, err)
	}

	env := Envelope{
		ReleaseID: rel.TagName,
		URL:       binURL,
		SHA256:    sum,
		Commit:    commit,
		// The release's published date feeds the never-downgrade
		// guard downstream as the candidate build time.
		BuildTime: rel.PublishedAt,
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// releaseAssetNames derives this platform's binary + sha256 sidecar names
// for a release tag. The third return is the legacy Windows sidecar name
// (without .exe), used as a fallback when the primary sidecar is absent.
func releaseAssetNames(tag, goos, goarch string) (bin, sha, legacySHA string) {
	baseTag := strings.TrimSuffix(tag, "-dev")
	bin = fmt.Sprintf("outpost-%s-%s-%s", baseTag, goos, goarch)
	if goos == "windows" {
		bin += ".exe"
	}
	sha = bin + ".sha256"
	legacySHA = fmt.Sprintf("outpost-%s-%s-%s.sha256", baseTag, goos, goarch)
	return bin, sha, legacySHA
}

// hasAssets reports whether the release carries this platform's binary +
// sha256 sidecar (including the legacy Windows sidecar name).
func (r ghRelease) hasAssets(goos, goarch string) bool {
	bin, sha, legacy := releaseAssetNames(r.TagName, goos, goarch)
	if r.assetURL(bin) == "" {
		return false
	}
	return r.assetURL(sha) != "" || r.assetURL(legacy) != ""
}

// pickRelease chooses which release to upgrade to. With a known current
// version it lists releases and takes the NEWEST by published date among
// releases strictly newer than the running build — GitHub's "latest"
// pointer is never consulted because it ignores prereleases (the
// 2026-10-24 downgrade incident: "latest" v0.32.0 resolved over a newer rc.10-dev).
// Without a known current version it keeps the legacy "latest" behavior.
func (g GitHubSource) pickRelease(ctx context.Context, repo, goos, goarch string) (ghRelease, error) {
	if !semver.IsValid(g.Current.Version) {
		return g.fetchLatest(ctx, repo)
	}
	rels, err := g.fetchReleases(ctx, repo)
	if err != nil {
		return ghRelease{}, err
	}
	var best *ghRelease
	var bestTime time.Time
	newestOverall := ""
	var newestTime time.Time
	for i := range rels {
		rel := &rels[i]
		if rel.Draft || strings.TrimSpace(rel.TagName) == "" {
			continue
		}
		ts, terr := parseBuildTime(rel.PublishedAt)
		if terr != nil {
			continue // without a date there is no "newest" to prove
		}
		if newestOverall == "" || ts.After(newestTime) {
			newestOverall, newestTime = rel.TagName, ts
		}
		if decline, _ := shouldDeclinePulledTarget(g.Current, Envelope{ReleaseID: rel.TagName, BuildTime: rel.PublishedAt}); decline {
			continue
		}
		if !rel.hasAssets(goos, goarch) {
			continue // newest first, but only releases published for this platform count
		}
		if best == nil || ts.After(bestTime) {
			best, bestTime = rel, ts
		}
	}
	if best == nil {
		return ghRelease{}, fmt.Errorf("no published release is newer than running %s (newest published: %s) — deliberate rollback only via `outpost rollback`",
			g.Current.Version, newestOverall)
	}
	return *best, nil
}

// resolveOutpostPin names the outpost commit a bashy product tag ships. The
// pin is the go.mod `tool github.com/qiangli/outpost/cmd/outpost` directive
// at the tag (the required outpost version: a pseudo-version's revision, or
// a tag resolved on the outpost repo). Tags cut before go.mod carried the
// pin fall back to the legacy .sibling-pins file.
func (g GitHubSource) resolveOutpostPin(ctx context.Context, repo, tag string) (string, error) {
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := g.getJSON(ctx, g.base()+"/repos/"+repo+"/contents/go.mod?ref="+url.QueryEscape(tag), &file); err == nil && file.Encoding == "base64" {
		if data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", "")); err == nil {
			tools, err := gomod.Tools(data)
			if err != nil {
				return "", fmt.Errorf("parse go.mod at %s: %w", tag, err)
			}
			for _, t := range tools {
				if t.Package != outpostToolPackage {
					continue
				}
				if t.Commit != "" {
					return shortCommit(t.Commit), nil
				}
				if t.Version != "" {
					return g.resolveTagCommit(ctx, "qiangli/outpost", t.Version)
				}
			}
		}
	}
	return g.resolveLegacyOutpostPin(ctx, repo, tag)
}

// outpostToolPackage is the tool directive a bashy product go.mod pins.
const outpostToolPackage = "github.com/qiangli/outpost/cmd/outpost"

func (g GitHubSource) resolveLegacyOutpostPin(ctx context.Context, repo, tag string) (string, error) {
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := g.getJSON(ctx, g.base()+"/repos/"+repo+"/contents/.sibling-pins?ref="+url.QueryEscape(tag), &file); err != nil {
		return "", err
	}
	if file.Encoding != "base64" {
		return "", errors.New("unexpected sibling pins encoding")
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode sibling pins: %w", err)
	}
	pin := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "outpost" {
			continue
		}
		if pin != "" {
			return "", errors.New("duplicate outpost source pin")
		}
		pin = strings.TrimSpace(value)
		raw, err := hex.DecodeString(pin)
		if err != nil || len(raw) != 20 {
			return "", errors.New("invalid outpost source pin")
		}
	}
	if pin == "" {
		return "", errors.New("release has no outpost source pin")
	}
	return shortCommit(pin), nil
}

func (g GitHubSource) fetchLatest(ctx context.Context, repo string) (ghRelease, error) {
	var rel ghRelease
	url := g.base() + "/repos/" + repo + "/releases/latest"
	if err := g.getJSON(ctx, url, &rel); err != nil {
		return ghRelease{}, err
	}
	return rel, nil
}

// fetchReleases lists published releases (newest first from the API) for
// the newest-pick in pickRelease.
func (g GitHubSource) fetchReleases(ctx context.Context, repo string) ([]ghRelease, error) {
	var rels []ghRelease
	if err := g.getJSON(ctx, g.base()+"/repos/"+repo+"/releases?per_page=100", &rels); err != nil {
		return nil, err
	}
	return rels, nil
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
