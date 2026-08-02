package vknode

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	// NativeArtifactURLAnnotation names a release archive that vk-native
	// downloads and executes instead of resolving command[0] from host PATH.
	NativeArtifactURLAnnotation = "outpost.dhnt.io/native-artifact-url"

	// NativeArtifactSHA256Annotation is mandatory whenever an artifact URL is
	// present. It makes the remote release archive an immutable job input.
	NativeArtifactSHA256Annotation = "outpost.dhnt.io/native-artifact-sha256"

	// NativeArtifactPathAnnotation selects a regular file inside the archive. In
	// single-member mode it is the one file extracted; in whole-tree mode (see
	// NativeArtifactTreeAnnotation) it is the entrypoint executable's
	// slash-separated path within the extracted tree (e.g. "bin/runner").
	NativeArtifactPathAnnotation = "outpost.dhnt.io/native-artifact-path"

	// NativeArtifactTreeAnnotation, when "true", materializes the ENTIRE archive
	// tree on the host (preserving its relative layout) instead of a single
	// member. NativeArtifactPathAnnotation then names the entrypoint executable
	// within that tree. This is what lets an artifact ship a runner binary
	// alongside sibling files it needs at launch (e.g. pyproject.toml + uv.lock
	// for a hermetic-venv build) without those files ever being resolved from,
	// or leaking onto, the host outside the content-addressed cache.
	NativeArtifactTreeAnnotation = "outpost.dhnt.io/native-artifact-tree"

	maxNativeArtifactBytes = int64(1 << 30)
	nativeArtifactTimeout  = 10 * time.Minute
)

// Whole-tree extraction caps. Archive extraction is a decompression-bomb
// surface: the downloaded archive is already bounded by maxNativeArtifactBytes,
// but its EXPANDED form is not, so the extractor must bound its own output.
// These are vars (not consts) only so tests can lower them without writing
// multi-gigabyte fixtures.
var (
	nativeArtifactTreeMaxFileBytes  = maxNativeArtifactBytes // per extracted member
	nativeArtifactTreeMaxTotalBytes = int64(4 << 30)         // whole extracted tree
	nativeArtifactTreeMaxFiles      = 50000                  // member count (inode guard)
)

func (b *nativeProcessBackend) resolveCommand(ctx context.Context, pod *corev1.Pod, name string) (string, error) {
	artifactURL := strings.TrimSpace(pod.Annotations[NativeArtifactURLAnnotation])
	artifactSHA := strings.TrimSpace(pod.Annotations[NativeArtifactSHA256Annotation])
	artifactPath := strings.TrimSpace(pod.Annotations[NativeArtifactPathAnnotation])
	profileName := strings.TrimSpace(pod.Annotations[NativeArtifactCredentialProfileAnnotation])
	treeMode := strings.EqualFold(strings.TrimSpace(pod.Annotations[NativeArtifactTreeAnnotation]), "true")
	if artifactURL == "" && artifactSHA == "" && artifactPath == "" && profileName == "" && !treeMode {
		return b.lookPath(name)
	}
	if artifactURL == "" || artifactSHA == "" || artifactPath == "" {
		return "", errors.New("vknode: native artifact requires url, sha256, and path annotations")
	}
	var profile *NativeArtifactCredentialProfile
	if profileName != "" {
		if b.artifactCredentials == nil {
			return "", fmt.Errorf("vknode: native artifact credential profile %q unavailable", profileName)
		}
		resolved, err := b.artifactCredentials.ResolveNativeArtifactCredential(ctx, pod.Namespace, profileName)
		if err != nil {
			// Resolver implementations may be backed by external brokers.
			// Keep their response details out of Pod status and evidence.
			return "", fmt.Errorf("vknode: native artifact credential profile %q unavailable", profileName)
		}
		profile = &resolved
	}
	if treeMode {
		return b.materializeNativeArtifactTreeWithCredential(
			ctx, artifactURL, artifactSHA, artifactPath, profile,
		)
	}
	return b.materializeNativeArtifactWithCredential(
		ctx, artifactURL, artifactSHA, artifactPath, profile,
	)
}

func (b *nativeProcessBackend) materializeNativeArtifact(ctx context.Context, rawURL, wantSHA, member string) (string, error) {
	return b.materializeNativeArtifactWithCredential(ctx, rawURL, wantSHA, member, nil)
}

func (b *nativeProcessBackend) materializeNativeArtifactWithCredential(
	ctx context.Context,
	rawURL string,
	wantSHA string,
	member string,
	profile *NativeArtifactCredentialProfile,
) (string, error) {
	u, wantSHA, err := prepareNativeArtifactURL(rawURL, wantSHA, profile)
	if err != nil {
		return "", err
	}
	cleanMember, err := cleanNativeArtifactMember(member)
	if err != nil {
		return "", err
	}

	cacheRoot := filepath.Join(b.dataDir, "artifacts")
	suffix := ""
	if strings.EqualFold(filepath.Ext(cleanMember), ".exe") {
		suffix = ".exe"
	}
	memberKey := sha256.Sum256([]byte(cleanMember))
	finalDir := filepath.Join(cacheRoot, wantSHA+"-"+hex.EncodeToString(memberKey[:8]))
	finalPath := filepath.Join(finalDir, "executable"+suffix)
	if info, statErr := os.Stat(finalPath); statErr == nil && info.Mode().IsRegular() {
		return finalPath, nil
	}
	tmpDir, err := b.newNativeArtifactStagingDir(cacheRoot)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "archive")
	if err := downloadVerifiedArtifact(
		ctx, b.artifactHTTPClient, u, wantSHA, archivePath, profile,
	); err != nil {
		return "", err
	}
	stagedPath := filepath.Join(tmpDir, "executable"+suffix)
	switch {
	case isZipArtifactURL(u):
		err = extractZipMember(archivePath, cleanMember, stagedPath)
	case isTarGzipArtifactURL(u):
		err = extractTarGzipMember(archivePath, cleanMember, stagedPath)
	default:
		err = errNativeArtifactKind
	}
	if err != nil {
		return "", err
	}
	if err := os.Chmod(stagedPath, 0o700); err != nil {
		return "", fmt.Errorf("vknode: make native artifact executable: %w", err)
	}
	if err := os.Remove(archivePath); err != nil {
		return "", fmt.Errorf("vknode: remove staged native archive: %w", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		if info, statErr := os.Stat(finalPath); statErr == nil && info.Mode().IsRegular() {
			return finalPath, nil
		}
		return "", fmt.Errorf("vknode: publish native artifact: %w", err)
	}
	return finalPath, nil
}

// materializeNativeArtifactTree materializes the WHOLE archive tree on the host
// and returns the absolute path of the entrypoint executable within it. It rides
// the SAME URL-validate → download → sha256-verify path as the single-member
// form (prepareNativeArtifactURL + downloadVerifiedArtifact); only the
// extraction step and the cache key differ. There is deliberately no second
// download/verify mechanism — see docs/peer-dks-native-artifact.md.
func (b *nativeProcessBackend) materializeNativeArtifactTree(
	ctx context.Context, rawURL, wantSHA, entrypoint string,
) (string, error) {
	return b.materializeNativeArtifactTreeWithCredential(ctx, rawURL, wantSHA, entrypoint, nil)
}

func (b *nativeProcessBackend) materializeNativeArtifactTreeWithCredential(
	ctx context.Context,
	rawURL string,
	wantSHA string,
	entrypoint string,
	profile *NativeArtifactCredentialProfile,
) (string, error) {
	u, wantSHA, err := prepareNativeArtifactURL(rawURL, wantSHA, profile)
	if err != nil {
		return "", err
	}
	cleanEntry, err := cleanNativeArtifactMember(entrypoint)
	if err != nil {
		return "", err
	}

	cacheRoot := filepath.Join(b.dataDir, "artifacts")
	// Content-addressed by the archive digest ALONE: an identical archive yields
	// an identical tree, so re-extraction is skipped and different entrypoints
	// selecting into the same archive share one cached tree.
	finalDir := filepath.Join(cacheRoot, "tree-"+wantSHA)
	finalPath := filepath.Join(finalDir, filepath.FromSlash(cleanEntry))
	if info, statErr := os.Stat(finalPath); statErr == nil && info.Mode().IsRegular() {
		return finalPath, nil
	}
	tmpDir, err := b.newNativeArtifactStagingDir(cacheRoot)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "archive")
	if err := downloadVerifiedArtifact(
		ctx, b.artifactHTTPClient, u, wantSHA, archivePath, profile,
	); err != nil {
		return "", err
	}

	// Extract into a private subdir of the staging area, then atomically rename
	// the finished subdir into place. A partially-extracted tree is therefore
	// never reachable under finalDir — an interrupted run leaves only the
	// .materialize-* temp, which the defer removes.
	treeDir := filepath.Join(tmpDir, "tree")
	if err := os.Mkdir(treeDir, 0o700); err != nil {
		return "", fmt.Errorf("vknode: create native artifact tree dir: %w", err)
	}
	switch {
	case isZipArtifactURL(u):
		err = extractNativeArtifactTreeZip(archivePath, treeDir)
	case isTarGzipArtifactURL(u):
		err = extractNativeArtifactTreeTarGzip(archivePath, treeDir)
	default:
		err = errNativeArtifactKind
	}
	if err != nil {
		return "", err
	}

	stagedEntry := filepath.Join(treeDir, filepath.FromSlash(cleanEntry))
	info, statErr := os.Stat(stagedEntry)
	if statErr != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("vknode: native artifact entrypoint %q is not a regular file in the archive tree", cleanEntry)
	}
	if err := os.Chmod(stagedEntry, 0o700); err != nil {
		return "", fmt.Errorf("vknode: make native artifact entrypoint executable: %w", err)
	}
	if err := os.Rename(treeDir, finalDir); err != nil {
		// A concurrent materialization of the same digest may have published
		// first; a digest mismatch never lands here (download fails loudly).
		if info, statErr := os.Stat(finalPath); statErr == nil && info.Mode().IsRegular() {
			return finalPath, nil
		}
		return "", fmt.Errorf("vknode: publish native artifact tree: %w", err)
	}
	return finalPath, nil
}

var errNativeArtifactKind = errors.New(
	"vknode: native artifact must be a .tar.gz, .tgz, or .zip archive")

func isZipArtifactURL(u *url.URL) bool {
	return strings.HasSuffix(strings.ToLower(u.Path), ".zip")
}

func isTarGzipArtifactURL(u *url.URL) bool {
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz")
}

// prepareNativeArtifactURL is the ONE gate every artifact fetch passes: it
// decodes+validates the pinned sha256, enforces the transport (https, or
// loopback http) with no userinfo/query/fragment, and applies the credential
// profile's scope. Shared by single-member and whole-tree materialization so
// neither can drift from the other's trust checks.
func prepareNativeArtifactURL(
	rawURL, wantSHA string, profile *NativeArtifactCredentialProfile,
) (*url.URL, string, error) {
	wantSHA = strings.ToLower(strings.TrimSpace(wantSHA))
	sumBytes, err := hex.DecodeString(wantSHA)
	if err != nil || len(sumBytes) != sha256.Size {
		return nil, "", fmt.Errorf("vknode: native artifact sha256 must be 64 hexadecimal characters")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("vknode: parse native artifact URL: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, "", fmt.Errorf("vknode: native artifact URL must use https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, "", fmt.Errorf("vknode: native artifact URL must not contain userinfo, query, or fragment")
	}
	if profile != nil {
		if err := authorizeNativeArtifactURL(u, profile); err != nil {
			return nil, "", err
		}
	}
	return u, wantSHA, nil
}

// cleanNativeArtifactMember normalizes and refuses a member/entrypoint path that
// is absolute or climbs out of the archive root.
func cleanNativeArtifactMember(member string) (string, error) {
	cleanMember := path.Clean(strings.ReplaceAll(member, "\\", "/"))
	if cleanMember == "." || cleanMember == ".." ||
		strings.HasPrefix(cleanMember, "../") || path.IsAbs(cleanMember) {
		return "", fmt.Errorf("vknode: invalid native artifact path %q", member)
	}
	return cleanMember, nil
}

func (b *nativeProcessBackend) newNativeArtifactStagingDir(cacheRoot string) (string, error) {
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", fmt.Errorf("vknode: create native artifact cache: %w", err)
	}
	tmpDir, err := os.MkdirTemp(cacheRoot, ".materialize-*")
	if err != nil {
		return "", fmt.Errorf("vknode: create native artifact staging dir: %w", err)
	}
	return tmpDir, nil
}

func defaultNativeArtifactHTTPClient() *http.Client {
	return &http.Client{Timeout: nativeArtifactTimeout}
}

func downloadVerifiedArtifact(
	ctx context.Context,
	baseClient *http.Client,
	artifactURL *url.URL,
	wantSHA string,
	dst string,
	profile *NativeArtifactCredentialProfile,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL.String(), nil)
	if err != nil {
		return fmt.Errorf("vknode: create native artifact request: %w", err)
	}
	client := baseClient
	if client == nil {
		client = defaultNativeArtifactHTTPClient()
	}
	if profile != nil {
		if err := signNativeArtifactRequest(req, profile, time.Now().UTC()); err != nil {
			return err
		}
		// Never replay a credential onto a redirect target. Returning the 3xx
		// response also keeps the original query-free URL as the complete,
		// auditable scope of this fetch.
		copyClient := *client
		copyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &copyClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("vknode: download native artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.Request == nil || (resp.Request.URL.Scheme != "https" &&
		!(resp.Request.URL.Scheme == "http" && isLoopbackHost(resp.Request.URL.Hostname()))) {
		return fmt.Errorf("vknode: native artifact redirect downgraded to unverified transport")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vknode: download native artifact: HTTP %s", resp.Status)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("vknode: create native artifact archive: %w", err)
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(resp.Body, maxNativeArtifactBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("vknode: download native artifact: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("vknode: close native artifact archive: %w", closeErr)
	}
	if n > maxNativeArtifactBytes {
		return fmt.Errorf("vknode: native artifact exceeds %d bytes", maxNativeArtifactBytes)
	}
	gotSHA := hex.EncodeToString(hash.Sum(nil))
	if gotSHA != wantSHA {
		return fmt.Errorf("vknode: native artifact checksum mismatch: got %s, want %s", gotSHA, wantSHA)
	}
	return nil
}

func authorizeNativeArtifactURL(u *url.URL, profile *NativeArtifactCredentialProfile) error {
	if profile == nil || profile.Scope == nil {
		return errors.New("vknode: native artifact credential profile has no scope")
	}
	if profile.Kind != nativeArtifactProfileKindAWSSigV4 {
		return errors.New("vknode: native artifact credential profile kind is unsupported")
	}
	scope := profile.Scope
	if !strings.EqualFold(u.Scheme, scope.Scheme) ||
		!strings.EqualFold(u.Host, scope.Host) {
		return errors.New("vknode: native artifact URL is outside credential profile scope")
	}
	requestPath := cleanURLPath(u.Path)
	scopePath := cleanURLPath(scope.Path)
	if scopePath != "/" && requestPath != scopePath &&
		!strings.HasPrefix(requestPath, strings.TrimSuffix(scopePath, "/")+"/") {
		return errors.New("vknode: native artifact URL is outside credential profile scope")
	}
	return nil
}

func cleanURLPath(p string) string {
	if p == "" {
		return "/"
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(p, "/"))
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

func signNativeArtifactRequest(
	req *http.Request,
	profile *NativeArtifactCredentialProfile,
	now time.Time,
) error {
	if req == nil || req.URL == nil || profile == nil {
		return errors.New("vknode: cannot sign native artifact request")
	}
	if profile.AccessKey == "" || profile.SecretKey == "" || profile.Region == "" {
		return errors.New("vknode: native artifact credential profile is incomplete")
	}
	const service = "s3"
	emptyPayloadSHA := sha256.Sum256(nil)
	payloadSHA := hex.EncodeToString(emptyPayloadSHA[:])
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadSHA + "\n" +
		"x-amz-date:" + amzDate + "\n"
	canonicalRequest := req.Method + "\n" + canonicalURI + "\n\n" +
		canonicalHeaders + "\n" + signedHeaders + "\n" + payloadSHA
	scope := date + "/" + profile.Region + "/" + service + "/aws4_request"
	requestSHA := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" +
		hex.EncodeToString(requestSHA[:])
	dateKey := hmacSHA256([]byte("AWS4"+profile.SecretKey), date)
	regionKey := hmacSHA256(dateKey, profile.Region)
	serviceKey := hmacSHA256(regionKey, service)
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("x-amz-content-sha256", payloadSHA)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+profile.AccessKey+"/"+scope+
			", SignedHeaders="+signedHeaders+", Signature="+signature)
	return nil
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func extractTarGzipMember(archivePath, member, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("vknode: open native tar.gz artifact: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("vknode: read native tar.gz artifact: %w", nextErr)
		}
		// The signed contract names one exact member. Do not accept archive
		// aliases such as "./bin/runner" or "tmp/../bin/runner".
		if strings.ReplaceAll(hdr.Name, "\\", "/") != member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return fmt.Errorf("vknode: native artifact path %q is not a regular file", member)
		}
		return writeArchiveMember(dst, tr)
	}
	return fmt.Errorf("vknode: native artifact path %q not found", member)
}

func extractZipMember(archivePath, member, dst string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("vknode: open native zip artifact: %w", err)
	}
	defer zr.Close()
	for _, file := range zr.File {
		if strings.ReplaceAll(file.Name, "\\", "/") != member {
			continue
		}
		if !file.FileInfo().Mode().IsRegular() {
			return fmt.Errorf("vknode: native artifact path %q is not a regular file", member)
		}
		src, err := file.Open()
		if err != nil {
			return fmt.Errorf("vknode: open native zip member: %w", err)
		}
		writeErr := writeArchiveMember(dst, src)
		closeErr := src.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	return fmt.Errorf("vknode: native artifact path %q not found", member)
}

func writeArchiveMember(dst string, src io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("vknode: create native artifact executable: %w", err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(src, maxNativeArtifactBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("vknode: extract native artifact executable: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("vknode: close native artifact executable: %w", closeErr)
	}
	if n > maxNativeArtifactBytes {
		return fmt.Errorf("vknode: native artifact executable exceeds %d bytes", maxNativeArtifactBytes)
	}
	return nil
}

// extractNativeArtifactTreeTarGzip unpacks an entire .tar.gz/.tgz into destDir,
// preserving its relative layout. Every member is adversarial input: paths are
// confined to destDir, symlink/hardlink members can never become a write
// primitive outside it, output size is bounded, and file modes are sanitized to
// owner-only permissions (never setuid/setgid/sticky, never group/other write).
func extractNativeArtifactTreeTarGzip(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("vknode: open native tar.gz artifact: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	var files int
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("vknode: read native tar.gz artifact: %w", nextErr)
		}
		// pax extended/global headers carry metadata for the NEXT entry, not a
		// tree member; archive/tar folds their fields in for us, so skip them.
		if hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		target, err := safeNativeArtifactTreeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue // the archive-root entry itself
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("vknode: create native artifact dir: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			files++
			if files > nativeArtifactTreeMaxFiles {
				return fmt.Errorf("vknode: native artifact tree exceeds %d files", nativeArtifactTreeMaxFiles)
			}
			// A lying-small header cannot help an attacker (LimitReader below is
			// the real guard), but a lying-LARGE declared size is refused up
			// front so an obvious decompression bomb never starts streaming.
			if hdr.Size > nativeArtifactTreeMaxFileBytes {
				return fmt.Errorf("vknode: native artifact member %q declares %d bytes, over the %d-byte limit",
					hdr.Name, hdr.Size, nativeArtifactTreeMaxFileBytes)
			}
			n, err := writeNativeArtifactTreeFile(target, tr, hdr.FileInfo().Mode(), nativeArtifactTreeMaxTotalBytes-total)
			if err != nil {
				return err
			}
			total += n
		case tar.TypeSymlink:
			if err := writeNativeArtifactTreeSymlink(destDir, target, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeLink:
			// A hardlink to an existing host file would be a read/write handle on
			// a file outside the archive's own content. Native artifacts have no
			// need for them; refuse rather than validate a fragile linkname.
			return fmt.Errorf("vknode: native artifact member %q is a hardlink; refused", hdr.Name)
		default:
			return fmt.Errorf("vknode: native artifact member %q has unsupported type %q; refused",
				hdr.Name, string(hdr.Typeflag))
		}
	}
	return nil
}

func extractNativeArtifactTreeZip(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("vknode: open native zip artifact: %w", err)
	}
	defer zr.Close()
	var total int64
	var files int
	for _, zf := range zr.File {
		target, err := safeNativeArtifactTreeJoin(destDir, zf.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		mode := zf.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			link, err := readNativeArtifactZipSymlink(zf)
			if err != nil {
				return err
			}
			if err := writeNativeArtifactTreeSymlink(destDir, target, link); err != nil {
				return err
			}
		case zf.FileInfo().IsDir():
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("vknode: create native artifact dir: %w", err)
			}
		case mode.IsRegular():
			files++
			if files > nativeArtifactTreeMaxFiles {
				return fmt.Errorf("vknode: native artifact tree exceeds %d files", nativeArtifactTreeMaxFiles)
			}
			if zf.UncompressedSize64 > uint64(nativeArtifactTreeMaxFileBytes) {
				return fmt.Errorf("vknode: native artifact member %q declares %d bytes, over the %d-byte limit",
					zf.Name, zf.UncompressedSize64, nativeArtifactTreeMaxFileBytes)
			}
			rc, err := zf.Open()
			if err != nil {
				return fmt.Errorf("vknode: open native zip member: %w", err)
			}
			n, werr := writeNativeArtifactTreeFile(target, rc, mode, nativeArtifactTreeMaxTotalBytes-total)
			rc.Close()
			if werr != nil {
				return werr
			}
			total += n
		default:
			// Devices, fifos, sockets: refuse anything that is not a plain file,
			// directory, or in-tree symlink.
			return fmt.Errorf("vknode: native artifact member %q has unsupported mode %v; refused", zf.Name, mode)
		}
	}
	return nil
}

// writeNativeArtifactTreeFile writes one member, bounded by remaining, with the
// mode sanitized to owner-only. It opens O_EXCL so it will never write THROUGH a
// path a prior member already created — in particular a symlink — which is the
// load-bearing half of the symlink-write-through defense.
func writeNativeArtifactTreeFile(target string, src io.Reader, mode os.FileMode, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, fmt.Errorf("vknode: native artifact tree exceeds %d bytes", nativeArtifactTreeMaxTotalBytes)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, fmt.Errorf("vknode: create native artifact dir: %w", err)
	}
	perm := sanitizeNativeArtifactFileMode(mode)
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return 0, fmt.Errorf("vknode: create native artifact file: %w", err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(src, remaining+1))
	closeErr := out.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("vknode: extract native artifact file: %w", copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("vknode: close native artifact file: %w", closeErr)
	}
	if n > remaining {
		return 0, fmt.Errorf("vknode: native artifact tree exceeds %d bytes", nativeArtifactTreeMaxTotalBytes)
	}
	// A umask could have masked bits at open time; set the sanitized perm
	// explicitly so an executable member stays executable.
	if err := os.Chmod(target, perm); err != nil {
		return 0, fmt.Errorf("vknode: set native artifact file mode: %w", err)
	}
	return n, nil
}

// writeNativeArtifactTreeSymlink creates an in-tree symlink or refuses one whose
// target escapes destDir. Refusing the escaping link at CREATION is what stops a
// later member from writing through it to an out-of-tree location; a dangling
// but in-tree link is harmless and allowed.
func writeNativeArtifactTreeSymlink(destDir, target, linkname string) error {
	ln := strings.ReplaceAll(linkname, "\\", "/")
	if ln == "" {
		return fmt.Errorf("vknode: native artifact symlink has an empty target; refused")
	}
	if path.IsAbs(ln) || filepath.IsAbs(linkname) {
		return fmt.Errorf("vknode: native artifact symlink target %q is absolute; refused", linkname)
	}
	// Resolve the (possibly ../-laden) link against the link's own directory and
	// require the result to remain inside destDir.
	resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(ln))
	if !isWithinNativeArtifactDir(destDir, resolved) {
		return fmt.Errorf("vknode: native artifact symlink %q escapes the artifact tree; refused", linkname)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("vknode: create native artifact dir: %w", err)
	}
	// O_EXCL semantics: a duplicate member at this path makes Symlink fail rather
	// than clobber, so a benign entry cannot be swapped for a link after the fact.
	if err := os.Symlink(filepath.FromSlash(ln), target); err != nil {
		return fmt.Errorf("vknode: create native artifact symlink: %w", err)
	}
	return nil
}

func readNativeArtifactZipSymlink(zf *zip.File) (string, error) {
	if zf.UncompressedSize64 > 4096 {
		return "", fmt.Errorf("vknode: native artifact symlink %q target is too long; refused", zf.Name)
	}
	rc, err := zf.Open()
	if err != nil {
		return "", fmt.Errorf("vknode: open native zip symlink: %w", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return "", fmt.Errorf("vknode: read native zip symlink: %w", err)
	}
	return string(b), nil
}

// safeNativeArtifactTreeJoin confines an archive member's path to destDir. It
// returns ("", nil) for the archive-root entry (caller skips) and an error for
// any absolute path, ".." escape, or result that lands outside destDir.
func safeNativeArtifactTreeJoin(destDir, name string) (string, error) {
	norm := strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	norm = strings.TrimSuffix(norm, "/")
	if norm == "" || norm == "." {
		return "", nil
	}
	if path.IsAbs(norm) {
		return "", fmt.Errorf("vknode: native artifact member %q has an absolute path; refused", name)
	}
	for _, el := range strings.Split(norm, "/") {
		if el == ".." {
			return "", fmt.Errorf("vknode: native artifact member %q escapes the archive root; refused", name)
		}
	}
	target := filepath.Join(destDir, filepath.FromSlash(norm))
	if !isWithinNativeArtifactDir(destDir, target) {
		return "", fmt.Errorf("vknode: native artifact member %q escapes the archive root; refused", name)
	}
	return target, nil
}

func isWithinNativeArtifactDir(dir, target string) bool {
	dir = filepath.Clean(dir)
	target = filepath.Clean(target)
	if target == dir {
		return true
	}
	return strings.HasPrefix(target, dir+string(os.PathSeparator))
}

// sanitizeNativeArtifactFileMode reduces an archive member's mode to owner-only
// permissions. mode.Perm() already drops setuid/setgid/sticky; we keep only the
// owner's read/write and (if the archive marked it executable) the owner's
// execute bit, so the cached tree is never group/other-accessible.
func sanitizeNativeArtifactFileMode(mode os.FileMode) os.FileMode {
	perm := os.FileMode(0o600)
	if mode.Perm()&0o100 != 0 {
		perm |= 0o100
	}
	return perm
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}
