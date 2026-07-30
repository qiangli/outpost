package recipebuilder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// 64 KiB raw encodes to <88 KiB, leaving room for recipe metadata inside
	// cloudbox's existing 100,000-character Recipe.Content column.
	maxInlineArchiveBytes = 64 << 10
	maxExpandedBytes      = 16 << 20 // stop compression bombs
	maxArchiveEntries     = 4096
)

func validSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func safeRelativePath(name string) bool {
	if name == "" || filepath.IsAbs(name) {
		return false
	}
	clean := filepath.Clean(name)
	return clean != "." && clean != ".." &&
		!strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// Archive names always use slash separators, regardless of the host OS. Reject
// backslashes too so an archive accepted on Unix cannot become traversal if the
// same recipe is later consumed by a Windows implementation.
func safeArchivePath(name string) bool {
	if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return false
	}
	clean := path.Clean(name)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func decodeInlineArchive(encoded, wantDigest string) ([]byte, error) {
	if len(encoded) > base64.StdEncoding.EncodedLen(maxInlineArchiveBytes) {
		return nil, fmt.Errorf("inline context exceeds %d bytes", maxInlineArchiveBytes)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode inline context: %w", err)
	}
	if len(raw) > maxInlineArchiveBytes {
		return nil, fmt.Errorf("inline context exceeds %d bytes", maxInlineArchiveBytes)
	}
	got := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(got[:]), wantDigest) {
		return nil, fmt.Errorf("inline context sha256 mismatch")
	}
	return raw, nil
}

// extractInlineArchive verifies the compressed/archive bytes before parsing,
// then admits regular files and directories only. Links and special files are
// rejected because their targets can escape after extraction.
func extractInlineArchive(raw []byte, dest string) error {
	var (
		reader io.Reader = bytes.NewReader(raw)
		gz     *gzip.Reader
		err    error
	)
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gz, err = gzip.NewReader(reader)
		if err != nil {
			return fmt.Errorf("open gzip context: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	tr := tar.NewReader(reader)
	var expanded int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read context archive: %w", err)
		}
		entries++
		if entries > maxArchiveEntries {
			return fmt.Errorf("context archive exceeds %d entries", maxArchiveEntries)
		}
		if !safeArchivePath(hdr.Name) {
			return fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		target := filepath.Join(dest, filepath.Clean(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || expanded > maxExpandedBytes-hdr.Size {
				return fmt.Errorf("expanded context exceeds %d bytes", maxExpandedBytes)
			}
			expanded += hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, tr, hdr.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return fmt.Errorf("extract %q: %w", hdr.Name, copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}
