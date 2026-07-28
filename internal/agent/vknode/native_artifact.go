package vknode

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
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
)

func (b *nativeProcessBackend) resolveCommand(ctx context.Context, pod *corev1.Pod, name string) (string, error) {
	artifactURL := strings.TrimSpace(pod.Annotations[NativeArtifactURLAnnotation])
	artifactSHA := strings.TrimSpace(pod.Annotations[NativeArtifactSHA256Annotation])
	artifactPath := strings.TrimSpace(pod.Annotations[NativeArtifactPathAnnotation])
	if artifactURL == "" && artifactSHA == "" && artifactPath == "" {
		return b.lookPath(name)
	}
	if artifactURL == "" || artifactSHA == "" || artifactPath == "" {
		return "", errors.New("vknode: native artifact requires url, sha256, and path annotations")
	}
	return b.materializeNativeArtifact(ctx, artifactURL, artifactSHA, artifactPath)
}

func (b *nativeProcessBackend) materializeNativeArtifact(ctx context.Context, rawURL, wantSHA, member string) (string, error) {
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
	cleanMember := path.Clean(strings.ReplaceAll(member, "\\", "/"))
	if cleanMember == "." || cleanMember == ".." || strings.HasPrefix(cleanMember, "../") || path.IsAbs(cleanMember) {
		return "", fmt.Errorf("vknode: invalid native artifact path %q", member)
	}

	cacheRoot := filepath.Join(b.dataDir, "artifacts")
	suffix := ""
	if strings.EqualFold(filepath.Ext(cleanMember), ".exe") {
		suffix = ".exe"
	}
	finalDir := filepath.Join(cacheRoot, wantSHA)
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
	if err := downloadVerifiedArtifact(ctx, rawURL, wantSHA, archivePath); err != nil {
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

func downloadVerifiedArtifact(ctx context.Context, rawURL, wantSHA, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("vknode: create native artifact request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
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
		if path.Clean(strings.ReplaceAll(hdr.Name, "\\", "/")) != member {
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
		if path.Clean(strings.ReplaceAll(file.Name, "\\", "/")) != member {
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
