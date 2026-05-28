package vkpodman

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Build-source annotation namespace. When BuildSourceAnnotation is
// present on a Pod, vkpodman.CreatePod treats the spec image as a
// *target* tag (the image to produce) rather than a registry
// reference to pull. If that tag isn't already in the local podman
// image store, vkpodman clones the source, tars the build context,
// and POSTs it to the libpod build endpoint. No cloudbox-side state
// is involved — the source URL is the source of truth.
//
// Reproducibility across outposts is the operator's responsibility:
// pin BuildSourceAnnotation to an immutable git ref (tag or SHA),
// not a moving branch. Two outposts pulling at different times will
// produce identical images only when the ref is immutable.
//
// Privacy: the outpost runs `git clone <url>` with its own
// credentials (SSH keys for git+ssh URLs, HTTPS auth helpers for
// git+https URLs). Cloudbox never sees the source.
const (
	// BuildSourceAnnotation is git+https://... or git+ssh://... ,
	// optionally suffixed with @<ref>. Examples:
	//   git+https://github.com/user/repo@v1.2.3
	//   git+ssh://git@github.com/user/private@deadbeefcafe
	//   git+https://github.com/user/repo  (defaults to HEAD)
	BuildSourceAnnotation = "outpost.dhnt.io/build-source"

	// BuildDockerfileAnnotation is the path to the Dockerfile
	// relative to the build context. Defaults to "Dockerfile".
	BuildDockerfileAnnotation = "outpost.dhnt.io/build-dockerfile"

	// BuildContextAnnotation is the build context directory
	// relative to the git checkout root. Defaults to ".". Used when
	// the Dockerfile is in a subdirectory and shouldn't see the
	// rest of the repo as build context.
	BuildContextAnnotation = "outpost.dhnt.io/build-context"
)

// EnsureImageBuilt inspects pod for build annotations; when present
// and the image isn't already in podman's local store, clones the
// source and posts a build to libpod. Returns (built, err) — built
// is true when we just produced the image (caller can skip the
// PullImage step). false + nil err means "no build annotation OR
// image already cached" — the caller proceeds normally.
//
// Idempotent: subsequent CreatePods for the same (image-tag,
// already-cached) are zero-cost beyond the image-exists check.
func (p *Provider) EnsureImageBuilt(ctx context.Context, pod *corev1.Pod) (built bool, err error) {
	if pod == nil || pod.Annotations == nil {
		return false, nil
	}
	src := strings.TrimSpace(pod.Annotations[BuildSourceAnnotation])
	if src == "" {
		return false, nil
	}
	if len(pod.Spec.Containers) == 0 || strings.TrimSpace(pod.Spec.Containers[0].Image) == "" {
		return false, errors.New("vkpodman: build-source annotation set but no container image specified")
	}
	image := pod.Spec.Containers[0].Image

	// Image already present? Skip the build.
	if exists, err := p.client.ImageExists(ctx, image); err != nil {
		slog.Warn("vkpodman: image-exists probe failed (will attempt build)", "image", image, "err", err)
	} else if exists {
		return false, nil
	}

	cloneURL, ref, err := parseBuildSource(src)
	if err != nil {
		return false, err
	}
	dockerfile := strings.TrimSpace(pod.Annotations[BuildDockerfileAnnotation])
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	contextSub := strings.TrimSpace(pod.Annotations[BuildContextAnnotation])
	if contextSub == "" {
		contextSub = "."
	}

	tmp, err := os.MkdirTemp("", "outpost-build-*")
	if err != nil {
		return false, fmt.Errorf("vkpodman: mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmp)

	slog.Info("vkpodman: building image from source",
		"image", image, "git", cloneURL, "ref", ref,
		"dockerfile", dockerfile, "context", contextSub,
		"workdir", tmp)

	if err := gitClone(ctx, cloneURL, ref, tmp); err != nil {
		return false, fmt.Errorf("vkpodman: git clone %s: %w", cloneURL, err)
	}

	ctxDir := filepath.Join(tmp, contextSub)
	if _, err := os.Stat(ctxDir); err != nil {
		return false, fmt.Errorf("vkpodman: build context %q not in repo: %w", contextSub, err)
	}
	if _, err := os.Stat(filepath.Join(ctxDir, dockerfile)); err != nil {
		return false, fmt.Errorf("vkpodman: Dockerfile %q not in context: %w", dockerfile, err)
	}

	if err := p.client.BuildImage(ctx, ctxDir, dockerfile, image); err != nil {
		return false, fmt.Errorf("vkpodman: build %s: %w", image, err)
	}
	slog.Info("vkpodman: build complete", "image", image)
	return true, nil
}

// parseBuildSource accepts git+https://<host>/<path>[@<ref>] or
// git+ssh://[user@]<host>/<path>[@<ref>] and returns the clone URL
// (without the @ref suffix and without the git+ prefix) plus the
// ref. ref is "" when not specified, in which case git clone uses
// the remote's default branch.
func parseBuildSource(s string) (cloneURL, ref string, err error) {
	if !strings.HasPrefix(s, "git+") {
		return "", "", fmt.Errorf("build-source must be git+https:// or git+ssh:// (got %q)", s)
	}
	rest := strings.TrimPrefix(s, "git+")
	// Find the LAST @ — the URL itself may contain user@host. We
	// look at the path component only.
	u, err := url.Parse(rest)
	if err != nil {
		return "", "", fmt.Errorf("parse build-source %q: %w", s, err)
	}
	at := strings.LastIndex(u.Path, "@")
	if at >= 0 {
		ref = u.Path[at+1:]
		u.Path = u.Path[:at]
	}
	return u.String(), ref, nil
}

// gitClone shells out to `git clone --depth 1 --branch ref` (or a
// plain clone followed by checkout when ref is a SHA, since git
// can't shallow-clone an arbitrary SHA). The depth-1 path is the
// fast common case for tags/branches; the fallback handles SHAs
// at the cost of a full history fetch.
func gitClone(ctx context.Context, url, ref, dst string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("`git` not found in PATH — install git on this outpost to use build-source")
	}

	// Try shallow first (works for branch + tag refs, and HEAD).
	if ref == "" {
		c := exec.CommandContext(ctx, "git", "clone", "--depth", "1", url, dst)
		c.Stderr = os.Stderr
		return c.Run()
	}
	if c := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", ref, url, dst); c.Run() == nil {
		return nil
	}
	// Fallback for SHA refs — git refuses to shallow-clone an
	// arbitrary commit. Full clone + checkout.
	c := exec.CommandContext(ctx, "git", "clone", url, dst)
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return err
	}
	co := exec.CommandContext(ctx, "git", "-C", dst, "checkout", "--detach", ref)
	co.Stderr = os.Stderr
	return co.Run()
}

// BuildImage POSTs the directory at ctxDir as a tar stream to libpod
// /build, asking it to build using `dockerfile` relative to that
// context and tag the result as `image`. Returns nil on success, an
// error including the libpod build output on failure.
//
// .dockerignore handling is delegated to libpod's build engine — we
// stream the full directory; podman applies its own ignore rules.
// This matches `podman build` CLI semantics.
func (c *Client) BuildImage(ctx context.Context, ctxDir, dockerfile, image string) error {
	pr, pw := io.Pipe()
	// Tar in the background so we can stream into the POST body.
	go func() {
		_ = pw.CloseWithError(tarDir(ctxDir, pw))
	}()

	q := url.Values{}
	q.Set("dockerfile", dockerfile)
	q.Add("t", image)
	// Force a fresh build context — podman caches layers anyway, but
	// no-cache=false (the default) is what we want; layer reuse is the
	// point. Leaving the query untouched.

	endpoint := apiPrefix + "/libpod/build?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://localhost"+endpoint, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// libpod streams a JSON-per-line "stream"/"error" log. Read it
	// all so the caller sees the failure cause; we treat any non-
	// 200 response or any line with an "error" field as fatal.
	dec := json.NewDecoder(resp.Body)
	var lastErr string
	for {
		var line map[string]any
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("libpod build: read response: %w (last: %s)", err, lastErr)
		}
		if e, ok := line["error"].(string); ok && e != "" {
			lastErr = e
		}
		// Surface the streamed build output — operators want to see it.
		if s, ok := line["stream"].(string); ok && s != "" {
			slog.Debug("podman build", "out", strings.TrimRight(s, "\n"))
		}
	}
	if resp.StatusCode >= 400 || lastErr != "" {
		return fmt.Errorf("libpod build (HTTP %d): %s", resp.StatusCode, lastErr)
	}
	return nil
}

// ImageExists asks libpod whether the named image is in the local
// store. Used by EnsureImageBuilt to short-circuit a build when the
// tag is already present.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	endpoint := apiPrefix + "/libpod/images/" + url.PathEscape(image) + "/exists"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://localhost"+endpoint, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("libpod images/exists: HTTP %d", resp.StatusCode)
	}
}

// tarDir writes a tar stream of root and everything under it. Used
// to feed libpod's build endpoint with the build context. Symlinks
// are followed and their target contents included — same as
// `podman build` CLI; avoids surprising operators with "you forgot
// to dereference a symlink in your repo".
func tarDir(root string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip the .git directory — it can be huge and never
		// belongs in a build context.
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := os.Stat(path) // follow symlinks
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
}
