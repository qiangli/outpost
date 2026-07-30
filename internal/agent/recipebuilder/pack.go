package recipebuilder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// InlineRecipeSpec describes a generic, explicitly-whitelisted source context.
// It deliberately has no cloudbox-specific defaults: proprietary callers choose
// their own context root and includes without embedding those sources in
// outpost.
type InlineRecipeSpec struct {
	Name       string
	Tag        string
	LocalRef   string
	ContextDir string
	Dockerfile string
	Includes   []string
	BaseImages []string
}

// PackInlineRecipe writes a flat ImageRecipe YAML document containing a
// deterministic tar.gz context. Only explicitly listed paths are included.
func PackInlineRecipe(out io.Writer, spec InlineRecipeSpec) error {
	r := Recipe{
		Name: spec.Name, Tag: spec.Tag, LocalRef: spec.LocalRef,
		ContextType: "inline_tar", Dockerfile: spec.Dockerfile,
		// Non-empty placeholders let shared validation cover every field before
		// archive construction; the real digest/archive replace them below.
		ContextArchive: "pending",
		ContextSha256:  strings.Repeat("0", sha256.Size*2),
	}
	if err := r.validate(); err != nil {
		return err
	}
	if len(spec.Includes) == 0 {
		return fmt.Errorf("at least one --include is required")
	}
	root, err := filepath.Abs(spec.ContextDir)
	if err != nil {
		return fmt.Errorf("resolve context dir: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat context dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("context dir is not a directory")
	}

	archive, files, err := buildInlineArchive(root, spec.Includes)
	if err != nil {
		return err
	}
	if _, ok := files[filepath.ToSlash(filepath.Clean(spec.Dockerfile))]; !ok {
		return fmt.Errorf("dockerfile %q is not covered by --include", spec.Dockerfile)
	}
	sum := sha256.Sum256(archive)
	r.ContextArchive = encodeArchive(archive)
	r.ContextSha256 = fmt.Sprintf("%x", sum)

	var doc strings.Builder
	fmt.Fprintln(&doc, "apiVersion: dks/v1")
	fmt.Fprintln(&doc, "kind: ImageRecipe")
	fmt.Fprintf(&doc, "name: %s\n", r.Name)
	if r.Tag != "" {
		fmt.Fprintf(&doc, "tag: %s\n", r.Tag)
	}
	fmt.Fprintf(&doc, "local_ref: %s\n", r.LocalRef)
	fmt.Fprintln(&doc, "context_type: inline_tar")
	fmt.Fprintf(&doc, "context_archive_base64: %s\n", r.ContextArchive)
	fmt.Fprintf(&doc, "dockerfile: %s\n", r.Dockerfile)
	if len(spec.BaseImages) > 0 {
		for _, base := range spec.BaseImages {
			if strings.TrimSpace(base) == "" || strings.ContainsAny(base, " \t\r\n") {
				return fmt.Errorf("invalid base image %q", base)
			}
		}
		fmt.Fprintf(&doc, "base_images: %s\n", strings.Join(spec.BaseImages, " "))
	}
	fmt.Fprintf(&doc, "context_sha256: %s\n", r.ContextSha256)
	_, err = io.WriteString(out, doc.String())
	return err
}

func encodeArchive(raw []byte) string {
	return base64Encoding.EncodeToString(raw)
}

// Kept as a package variable so packing and consuming use exactly the same
// strict base64 alphabet without duplicating configuration.
var base64Encoding = base64.StdEncoding.Strict()

func buildInlineArchive(root string, includes []string) ([]byte, map[string]struct{}, error) {
	cleanIncludes := make([]string, 0, len(includes))
	for _, include := range includes {
		if !safeRelativePath(include) {
			return nil, nil, fmt.Errorf("include must be a safe relative path: %q", include)
		}
		cleanIncludes = append(cleanIncludes, filepath.Clean(include))
	}
	sort.Strings(cleanIncludes)

	var compressed bytes.Buffer
	gz, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return nil, nil, err
	}
	gz.Header.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	seen := make(map[string]struct{})
	var expanded int64
	entries := 0

	closeWithError := func(cause error) ([]byte, map[string]struct{}, error) {
		_ = tw.Close()
		_ = gz.Close()
		return nil, nil, cause
	}
	for _, include := range cleanIncludes {
		start := filepath.Join(root, include)
		if err := filepath.WalkDir(start, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(root, name)
			if err != nil || !safeRelativePath(rel) {
				return fmt.Errorf("path escapes context root: %q", name)
			}
			archiveName := filepath.ToSlash(rel)
			if _, ok := seen[archiveName]; ok {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
				return fmt.Errorf("unsupported source entry %q", archiveName)
			}
			entries++
			if entries > maxArchiveEntries {
				return fmt.Errorf("context exceeds %d entries", maxArchiveEntries)
			}
			if info.Mode().IsRegular() {
				if expanded > maxExpandedBytes-info.Size() {
					return fmt.Errorf("context exceeds %d expanded bytes", maxExpandedBytes)
				}
				expanded += info.Size()
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = archiveName
			hdr.ModTime = time.Unix(0, 0)
			hdr.AccessTime = time.Time{}
			hdr.ChangeTime = time.Time{}
			hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, err := os.Open(name)
				if err != nil {
					return err
				}
				_, copyErr := io.Copy(tw, f)
				closeErr := f.Close()
				if copyErr != nil {
					return copyErr
				}
				if closeErr != nil {
					return closeErr
				}
			}
			seen[archiveName] = struct{}{}
			return nil
		}); err != nil {
			return closeWithError(err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return nil, nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, nil, err
	}
	if compressed.Len() > maxInlineArchiveBytes {
		return nil, nil, fmt.Errorf("compressed context is %d bytes; maximum is %d", compressed.Len(), maxInlineArchiveBytes)
	}
	return compressed.Bytes(), seen, nil
}
