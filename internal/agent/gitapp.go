package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// gitCredentialTTL bounds how long a filled host credential is reused
// across requests. Short on purpose: `git credential fill` is cheap,
// and this caps how long a rotated or revoked token keeps working
// after the helper has forgotten it.
const gitCredentialTTL = 5 * time.Minute

// gitCredentialFillFunc obtains the Basic-auth pair for an https git
// upstream at request time. The production implementation runs
// `git credential fill` on the host; tests inject a fake.
type gitCredentialFillFunc func(ctx context.Context, host string) (username, password string, err error)

// gitCredentialFill runs `git credential fill` with protocol=https and
// host=<host> on stdin, then parses the username/password pair from
// its stdout. GIT_TERMINAL_PROMPT=0 keeps it from blocking on an
// interactive prompt when no helper holds the credential. Errors are
// deliberately generic — the command's output never flows into them,
// so a misbehaving helper can't leak the credential through an error
// string.
func gitCredentialFill(ctx context.Context, host string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", "credential", "fill")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git credential fill for host %q failed", host)
	}
	var user, pass string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "username="):
			user = strings.TrimPrefix(line, "username=")
		case strings.HasPrefix(line, "password="):
			pass = strings.TrimPrefix(line, "password=")
		}
	}
	if user == "" || pass == "" {
		return "", "", fmt.Errorf("git credential fill for host %q returned no credential", host)
	}
	return user, pass, nil
}

// gitApp is one git+https reverse-proxy app. Only the two smart-HTTP
// clone/fetch exchanges are forwarded; every other request shape is
// refused before the upstream — or the host credential — is touched.
type gitApp struct {
	target *url.URL // upstream base (scheme + host only)
	allow  []string // normalized "owner/repo" | "owner/*" entries
	mu     sync.Mutex
	user   string
	pass   string
	filled time.Time
}

// registerGit mounts a git app: /app/<name>/ proxies git smart-HTTP
// clone/fetch requests to the https upstream, restricted to the
// owner/repo allowlist and authenticated with the host credential. An
// empty allowlist refuses every request.
func (r *AppRegistry) registerGit(name string, target *url.URL, allow []string, meta AppMeta) error {
	normalized, err := conf.NormalizeGitAllow(allow)
	if err != nil {
		return fmt.Errorf("app %q: %w", name, err)
	}
	guard, guarded, err := parseExposeGuard(name, meta.ExposeTokenSHA256, meta.ExposeExpires)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A name is one of http/tcp/egress/git at a time; clear the other modes.
	delete(r.proxy, name)
	delete(r.tcp, name)
	delete(r.egress, name)
	r.apps[name] = target
	r.git[name] = &gitApp{target: target, allow: normalized}
	r.requireLogin[name] = meta.RequireLogin
	r.elevationRequired[name] = meta.ElevationRequired
	r.lanOnly[name] = append([]string(nil), meta.LANOnlyPaths...)
	r.indexPath[name] = meta.IndexPath
	r.capabilities[name] = meta.Capabilities
	r.trustCloudIdentity[name] = meta.TrustCloudIdentity
	r.setExpose(name, guard, guarded)
	return nil
}

// gitUpstreamClient forwards git smart-HTTP exchanges. No timeout: a
// fetch body can stream for minutes.
var gitUpstreamClient = &http.Client{}

// gitResponseHeaders is the allow-list of response headers copied back
// to the caller. Everything else (auth-ish headers, cookies, ...) is
// dropped so a hostile upstream can't smuggle data — let alone the
// credential it was sent — back through the proxy. Content-Length and
// Content-Encoding are deliberately excluded: the body is streamed
// through the redacting reader (which can change its length) after the
// bytes were negotiated uncompressed (see serve), so any upstream
// length or encoding label would mis-frame the stream and truncate the
// clone mid-pkt-line.
var gitResponseHeaders = []string{
	"Content-Type",
	"Cache-Control",
	"Pragma",
	"Expires",
}

// serve forwards one allowlisted clone/fetch exchange. Caller-supplied
// Authorization/Cookie headers are stripped; the outbound request
// carries only the git protocol headers plus the host credential as
// Basic auth.
func (g *gitApp) serve(c *gin.Context, name, rest string, fill gitCredentialFillFunc) {
	// Strip caller credentials first: the only Authorization this
	// upstream ever sees is the host credential injected below.
	c.Request.Header.Del("Authorization")
	c.Request.Header.Del("Cookie")

	path, query, ok := matchGitSmartRequest(c.Request.Method, rest, c.Request.URL.RawQuery, g.allow)
	if !ok {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	user, pass, err := g.credential(c.Request.Context(), fill)
	if err != nil {
		slog.Warn("git app credential unavailable", "app", name, "err", err)
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}

	out := *g.target
	out.Path = path
	out.RawQuery = query
	var body io.Reader
	if c.Request.Method == http.MethodPost {
		body = c.Request.Body
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, out.String(), body)
	if err != nil {
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Accept", "Content-Type", "Git-Protocol", "User-Agent"} {
		if v := c.Request.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	// Pass bytes through unmodified: without this the http.Transport
	// advertises gzip and transparently decompresses, while any length
	// framing copied from the upstream still describes the compressed
	// transfer — the stream the caller reads is cut short and the clone
	// dies mid-pkt-line. identity keeps both body and headers truthful.
	req.Header.Set("Accept-Encoding", "identity")
	if c.Request.Method == http.MethodPost && c.Request.ContentLength > 0 {
		req.ContentLength = c.Request.ContentLength
	}
	req.SetBasicAuth(user, pass)

	resp, err := gitUpstreamClient.Do(req)
	if err != nil {
		slog.Warn("git app upstream request failed", "app", name, "err", err)
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		// Credential rejected — drop the cache so the next request
		// refills from the helper (covers helper-side rotation).
		g.invalidate()
	}

	h := c.Writer.Header()
	for _, key := range gitResponseHeaders {
		for _, v := range resp.Header.Values(key) {
			h.Add(key, v)
		}
	}
	// Redirects pass through only when provably credential-free.
	if loc := resp.Header.Get("Location"); loc != "" && !containsGitSecret(loc, user, pass) {
		h.Set("Location", loc)
	}
	// Never frame the caller's response with an upstream length or
	// encoding: the body is streamed uncompressed through the redacting
	// reader, which may change it — a stale Content-Length would have
	// the caller (or a relay between us) cut the stream early.
	h.Del("Content-Length")
	h.Del("Content-Encoding")
	if resp.StatusCode >= 400 {
		// Error bodies are discarded wholesale: a hostile upstream
		// could otherwise echo the Authorization back through us.
		_, _ = io.Copy(io.Discard, resp.Body)
		c.AbortWithStatus(resp.StatusCode)
		return
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(c.Writer, newSecretRedactingReader(resp.Body, gitSecrets(user, pass)...))
}

// credential returns the cached host credential, refilling it via
// fill when the cache is empty or older than gitCredentialTTL. The
// fill runs under the app lock, so concurrent requests can't stampede
// the credential helper.
func (g *gitApp) credential(ctx context.Context, fill gitCredentialFillFunc) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.filled.IsZero() && time.Since(g.filled) < gitCredentialTTL {
		return g.user, g.pass, nil
	}
	user, pass, err := fill(ctx, g.target.Hostname())
	if err != nil {
		return "", "", err
	}
	if user == "" || pass == "" {
		return "", "", fmt.Errorf("credential filler returned an empty credential")
	}
	g.user, g.pass, g.filled = user, pass, time.Now()
	return user, pass, nil
}

// invalidate drops the cached credential (e.g. after a 401) so the
// next request refills from the helper.
func (g *gitApp) invalidate() {
	g.mu.Lock()
	g.user, g.pass, g.filled = "", "", time.Time{}
	g.mu.Unlock()
}

// matchGitSmartRequest validates one smart-HTTP clone/fetch exchange
// against the allowlist:
//
//	GET  /<owner>/<repo>[.git]/info/refs?service=git-upload-pack
//	POST /<owner>/<repo>[.git]/git-upload-pack
//
// Everything else — push (git-receive-pack), other endpoints, deeper
// paths, malformed segments, unlisted repos, or an empty allowlist —
// returns ok=false and the request never reaches the upstream. The
// returned path is rebuilt from the validated segments, so nothing the
// caller sent (traversal, extra segments, ...) can smuggle through.
func matchGitSmartRequest(method, rest, rawQuery string, allow []string) (path, query string, ok bool) {
	if len(allow) == 0 {
		return "", "", false
	}
	segs := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	var owner, repo, endpoint string
	switch {
	case method == http.MethodGet && len(segs) == 4 && segs[2] == "info" && segs[3] == "refs":
		endpoint = "info/refs"
	case method == http.MethodPost && len(segs) == 3 && segs[2] == "git-upload-pack":
		endpoint = "git-upload-pack"
	default:
		return "", "", false
	}
	for _, s := range segs {
		if !isGitSegment(s) {
			return "", "", false
		}
	}
	owner, repo = segs[0], segs[1]
	repoName := strings.TrimSuffix(repo, ".git")
	switch {
	case method == http.MethodGet && endpoint == "info/refs":
		q, err := url.ParseQuery(rawQuery)
		if err != nil || len(q["service"]) != 1 || q.Get("service") != "git-upload-pack" {
			return "", "", false
		}
		query = "service=git-upload-pack"
	case method == http.MethodPost && endpoint == "git-upload-pack":
		if rawQuery != "" {
			return "", "", false
		}
	default:
		return "", "", false
	}
	if !gitAllowMatches(allow, owner, repoName) {
		return "", "", false
	}
	return "/" + owner + "/" + repo + "/" + endpoint, query, true
}

// isGitSegment accepts the [-.A-Za-z0-9_] charset git hosts use for
// owner/repo names and the two endpoints, rejecting the dot-only
// names that could otherwise smuggle a traversal past the join.
func isGitSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// gitAllowMatches reports whether owner/repo is covered by an allow
// entry: "owner/repo" exact, "owner/*" every repo under owner. Code
// hosts treat owner and repo names case-insensitively; so does this
// match.
func gitAllowMatches(allow []string, owner, repo string) bool {
	for _, a := range allow {
		o, r, found := strings.Cut(a, "/")
		if !found || !strings.EqualFold(o, owner) {
			continue
		}
		if r == "*" || strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// gitSecrets lists every surface form of the credential that must
// never appear in a response: the raw password, the user:password
// pair, and its base64 Basic-auth encoding.
func gitSecrets(user, pass string) []string {
	return []string{
		pass,
		user + ":" + pass,
		base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)),
	}
}

func containsGitSecret(s, user, pass string) bool {
	for _, secret := range gitSecrets(user, pass) {
		if strings.Contains(s, secret) {
			return true
		}
	}
	return false
}

// newSecretRedactingReader streams r with every occurrence of the
// given secrets replaced by "[redacted]". Secrets can't span chunk
// boundaries undetected: the last maxLen-1 bytes of buffered data are
// held back until more arrives (or EOF flushes them).
func newSecretRedactingReader(r io.Reader, secrets ...string) io.Reader {
	maxLen := 0
	for _, s := range secrets {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}
	return &secretRedactingReader{r: r, secrets: secrets, keep: maxLen - 1, chunk: make([]byte, 32*1024)}
}

type secretRedactingReader struct {
	r       io.Reader
	secrets []string
	keep    int
	chunk   []byte
	// buf holds bytes not yet released to the caller: freshly read
	// data plus the held-back tail that a secret spanning into future
	// reads may still complete. The tail may already carry
	// "[redacted]" spans cut in half at the hold boundary.
	buf []byte
	// pending holds redacted-but-unsent bytes from a previous Read
	// whose p was too small; they are emitted verbatim, never run
	// through replace again.
	pending []byte
	eof     bool
}

func (rr *secretRedactingReader) Read(p []byte) (int, error) {
	if len(rr.pending) > 0 {
		n := copy(p, rr.pending)
		rr.pending = rr.pending[n:]
		return n, nil
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		// Redact every secret fully contained in the buffer, then
		// hold back the last keep bytes of the result: a secret
		// spanning into future data keeps its raw head inside that
		// tail, stays whole in buf, and is redacted once the rest
		// arrives (or flushed verbatim at EOF).
		redacted := rr.buf
		for _, s := range rr.secrets {
			if len(s) > 0 {
				redacted = bytes.ReplaceAll(redacted, []byte(s), []byte("[redacted]"))
			}
		}
		cut := len(redacted)
		if !rr.eof && rr.keep > 0 {
			cut -= min(rr.keep, cut)
		}
		if cut == 0 {
			if rr.eof {
				return 0, io.EOF
			}
			n, err := rr.r.Read(rr.chunk)
			if n > 0 {
				rr.buf = append(rr.buf, rr.chunk[:n]...)
			}
			if err == io.EOF {
				rr.eof = true
			} else if err != nil {
				return 0, err
			}
			continue
		}
		n := copy(p, redacted[:cut])
		rr.pending = redacted[n:cut]
		rr.buf = append([]byte(nil), redacted[cut:]...)
		return n, nil
	}
}
