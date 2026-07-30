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

	// NativeArtifactPathAnnotation selects one regular file inside the archive.
	NativeArtifactPathAnnotation = "outpost.dhnt.io/native-artifact-path"

	maxNativeArtifactBytes = int64(1 << 30)
	nativeArtifactTimeout  = 10 * time.Minute
)

func (b *nativeProcessBackend) resolveCommand(ctx context.Context, pod *corev1.Pod, name string) (string, error) {
	artifactURL := strings.TrimSpace(pod.Annotations[NativeArtifactURLAnnotation])
	artifactSHA := strings.TrimSpace(pod.Annotations[NativeArtifactSHA256Annotation])
	artifactPath := strings.TrimSpace(pod.Annotations[NativeArtifactPathAnnotation])
	profileName := strings.TrimSpace(pod.Annotations[NativeArtifactCredentialProfileAnnotation])
	if artifactURL == "" && artifactSHA == "" && artifactPath == "" && profileName == "" {
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
	wantSHA = strings.ToLower(strings.TrimSpace(wantSHA))
	sumBytes, err := hex.DecodeString(wantSHA)
	if err != nil || len(sumBytes) != sha256.Size {
		return "", fmt.Errorf("vknode: native artifact sha256 must be 64 hexadecimal characters")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("vknode: parse native artifact URL: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return "", fmt.Errorf("vknode: native artifact URL must use https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("vknode: native artifact URL must not contain userinfo, query, or fragment")
	}
	if profile != nil {
		if err := authorizeNativeArtifactURL(u, profile); err != nil {
			return "", err
		}
	}
	cleanMember := path.Clean(strings.ReplaceAll(member, "\\", "/"))
	if cleanMember == "." || cleanMember == ".." || strings.HasPrefix(cleanMember, "../") || path.IsAbs(cleanMember) {
		return "", fmt.Errorf("vknode: invalid native artifact path %q", member)
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
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", fmt.Errorf("vknode: create native artifact cache: %w", err)
	}
	tmpDir, err := os.MkdirTemp(cacheRoot, ".materialize-*")
	if err != nil {
		return "", fmt.Errorf("vknode: create native artifact staging dir: %w", err)
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
	case strings.HasSuffix(strings.ToLower(u.Path), ".zip"):
		err = extractZipMember(archivePath, cleanMember, stagedPath)
	case strings.HasSuffix(strings.ToLower(u.Path), ".tar.gz"), strings.HasSuffix(strings.ToLower(u.Path), ".tgz"):
		err = extractTarGzipMember(archivePath, cleanMember, stagedPath)
	default:
		err = fmt.Errorf("vknode: native artifact must be a .tar.gz, .tgz, or .zip archive")
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

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}
