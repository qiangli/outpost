package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// StageFromURL downloads the candidate binary at `srcURL` into `dst`,
// verifying the body against `expectedSHA` (hex) on the fly. Both
// caller-provided modes (cloudbox push and `outpost upgrade --from`)
// flow through here; the only difference is the daemon's worker
// always passes a non-empty expectedSHA while the CLI permits empty
// for ad-hoc test pushes from a local artifact server.
//
// The destination is opened O_EXCL so a stale "<binary>.upgrading"
// from a crashed prior attempt surfaces as an error instead of silent
// clobber. Caller is responsible for cleaning up `dst` on downstream
// errors.
func StageFromURL(ctx context.Context, dst, srcURL, expectedSHA string, client *http.Client) error {
	u, err := url.Parse(srcURL)
	if err != nil || u.Scheme != "https" {
		return errors.New("source URL must be https://")
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", srcURL, resp.StatusCode)
	}
	return writeAndVerify(dst, resp.Body, expectedSHA)
}

// StageFromLocal copies `srcPath` into `dst`, verifying sha256 if
// `expectedSHA` is non-empty. CLI-only entry point; the daemon worker
// never reads from a local path (callers can't be trusted to deliver
// arbitrary paths over the wire).
func StageFromLocal(srcPath, dst, expectedSHA string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeAndVerify(dst, f, expectedSHA)
}

// writeAndVerify is the shared sink: O_EXCL create of dst, stream-copy
// from src through sha256, then sha256 compare when expected is set.
// On any error, dst is left in place (whatever was written) — caller
// owns cleanup so a partial download doesn't get silently swapped in.
func writeAndVerify(dst string, src io.Reader, expectedSHA string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("stale upgrade candidate at %s — remove it and retry", dst)
		}
		return err
	}
	defer out.Close()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hasher), src); err != nil {
		return fmt.Errorf("copy candidate: %w", err)
	}
	if expectedSHA != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(strings.TrimSpace(expectedSHA), got) {
			return fmt.Errorf("sha256 mismatch: expected %s, got %s", expectedSHA, got)
		}
	}
	return nil
}
