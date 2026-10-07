package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Companion is an optional additional executable in the same release. Names
// are a closed set; remote input cannot select arbitrary destination paths.
type Companion struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}
type RollbackMember struct {
	SHA256     string `json:"sha256,omitempty"`
	BinaryPath string `json:"binary_path"`
	PrevPath   string `json:"prev_path"`
	Absent     bool   `json:"absent,omitempty"`
}
type pairRecord struct {
	BinaryPath string           `json:"binary_path"`
	Phase      string           `json:"phase"`
	Members    []RollbackMember `json:"members"`
}

func pairRecordPath(binary string) string { return binary + ".pair-rollback.json" }
func productName(name string) string      { return binmgr.BinaryName(name) }
func ProductMembers() []string {
	return []string{productName("bashy"), productName("bash"), productName("sh"), productName("outpost")}
}
func allowedCompanion(name string) bool {
	return name == productName("bashy") || name == productName("bash") || name == productName("sh")
}

// ParseBashyBanner extracts the product release from bashy and compatibility shells.
func ParseBashyBanner(s string) string {
	_, rest, ok := strings.Cut(s, "-bashy-")
	if !ok {
		return ""
	}
	if end := strings.IndexAny(rest, " \n\r\t()"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}
func productVersion(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "v"), "-dev")
}
func ProbeShell(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return fmt.Errorf("probe shell: %w", err)
	}
	have := ParseBashyBanner(string(out))
	if have == "" {
		return errors.New("shell has no bashy release stamp")
	}
	if version != "" && productVersion(have) != productVersion(version) {
		return fmt.Errorf("shell version %s does not match outpost %s", have, version)
	}
	return nil
}

// StageArchive stages all four executables together. The archive's checksum is
// verified by the caller before entering this local extraction path.
func StageArchive(archive, binary string) (string, map[string]string, func(), error) {
	dir, err := os.MkdirTemp(filepath.Dir(binary), ".outpost-pair-*")
	if err != nil {
		return "", nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	paths, err := binmgr.ExtractMembers(archive, dir, ProductMembers())
	if err != nil {
		cleanup()
		return "", nil, func() {}, err
	}
	candidate := paths[productName("outpost")]
	delete(paths, productName("outpost"))
	return candidate, paths, cleanup, nil
}

// StageReleasePair recognizes the one-product release URL. Legacy URLs remain
// single-binary; a recognized new release fails closed if its archive is absent
// or malformed. Both bytes and checksum come from the identical immutable tag.
func StageReleasePair(ctx context.Context, rawURL, binary string, client *http.Client) (string, map[string]string, func(), error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, func() {}, err
	}
	prefix := "/qiangli/bashy/releases/download/"
	if u.Host != "github.com" || !strings.HasPrefix(u.Path, prefix) {
		return "", nil, func() {}, nil
	}
	if u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" {
		return "", nil, func() {}, errors.New("invalid paired release URL")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, prefix), "/")
	if len(parts) != 2 || parts[0] == "" || !strings.HasPrefix(parts[1], "outpost-") {
		return "", nil, func() {}, errors.New("invalid paired outpost release asset")
	}
	base := "https://github.com" + prefix + url.PathEscape(parts[0]) + "/"
	archive := "bashy-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	if runtime.GOOS == "windows" {
		archive = "bashy-windows-" + runtime.GOARCH + ".zip"
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"checksums.txt", nil)
	if err != nil {
		return "", nil, func() {}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, func() {}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, func() {}, fmt.Errorf("release checksums: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, func() {}, err
	}
	digest := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archive {
			if digest != "" {
				return "", nil, func() {}, errors.New("duplicate archive checksum")
			}
			digest = fields[0]
		}
	}
	if len(digest) != 64 {
		return "", nil, func() {}, fmt.Errorf("missing sha256 for %s", archive)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(binary), ".outpost-release-*")
	if err != nil {
		return "", nil, func() {}, err
	}
	defer os.RemoveAll(tmp)
	path := filepath.Join(tmp, archive)
	if err := StageFromURL(ctx, path, base+archive, digest, client); err != nil {
		return "", nil, func() {}, err
	}
	return StageArchive(path, binary)
}

func readPairRecord(binary string) (*pairRecord, error) {
	data, err := os.ReadFile(pairRecordPath(binary))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec pairRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if (rec.Phase != "prepared" && rec.Phase != "complete") || rec.BinaryPath != binary || len(rec.Members) < 2 || len(rec.Members) > 4 {
		return nil, errors.New("invalid pair rollback record")
	}
	seen := map[string]bool{}
	for _, m := range rec.Members {
		if filepath.Dir(m.BinaryPath) != filepath.Dir(binary) || (filepath.Dir(m.PrevPath) != filepath.Dir(binary) || !strings.HasPrefix(filepath.Base(m.PrevPath), filepath.Base(m.BinaryPath)+".previous-")) || seen[m.BinaryPath] || (m.BinaryPath != binary && !allowedCompanion(filepath.Base(m.BinaryPath))) {
			return nil, errors.New("invalid pair rollback member")
		}
		seen[m.BinaryPath] = true
	}
	if !seen[binary] {
		return nil, errors.New("pair rollback record has no outpost")
	}
	return &rec, nil
}
func writePairRecord(rec pairRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := pairRecordPath(rec.BinaryPath)
	f, err := os.CreateTemp(filepath.Dir(path), ".pair-record-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

// ApplyPair stages/probes before mutation, retains every prior executable, then
// swaps userland first and outpost last. A durable record supports interrupted
// transaction recovery and later operator rollback, even after health confirmation.
type PairOptions struct{ ConfirmPath, ReleaseID, FromSHA string }

func ApplyPair(ctx context.Context, binary, candidate string, companions map[string]string, options ...PairOptions) (agent.BuildInfo, error) {
	build, err := Probe(candidate, "")
	if err != nil {
		return build, err
	}
	if len(companions) == 0 {
		return build, errors.New("paired upgrade requires companions")
	}
	if companions[productName("bashy")] == "" {
		return build, errors.New("paired upgrade requires bashy")
	}
	for name, path := range companions {
		if !allowedCompanion(name) {
			return build, fmt.Errorf("unsupported companion %q", name)
		}
		if err := ProbeShell(ctx, path, build.Version); err != nil {
			return build, fmt.Errorf("%s: %w", name, err)
		}
	}
	armed := false
	var beforeSwap func() error
	if len(options) > 0 && options[0].ConfirmPath != "" {
		opt := options[0]
		beforeSwap = func() error {
			err := WritePendingConfirm(opt.ConfirmPath, NewPendingConfirm(opt.ReleaseID, opt.FromSHA, build.Commit, binary, binary+".previous"))
			armed = err == nil
			return err
		}
	}
	err = applyPair(binary, candidate, companions, beforeSwap)
	if err != nil && armed && len(options) > 0 && options[0].ConfirmPath != "" {
		// Only clear after rollback completed. Failed recovery keeps both marker
		// and durable transaction so the supervisor can retry it.
		if rec, readErr := readPairRecord(binary); readErr == nil && (rec == nil || rec.Phase == "complete") {
			err = errors.Join(err, ClearPendingConfirm(options[0].ConfirmPath))
		}
	}
	return build, err
}

// applyPair is the file transaction, kept independent from executable probes so
// fault tests never execute the test program as a candidate.
func applyPair(binary, candidate string, companions map[string]string, beforeSwap func() error) error {
	prior, err := readPairRecord(binary)
	if err != nil {
		return err
	} else if prior != nil && prior.Phase == "prepared" {
		return errors.New("interrupted paired upgrade must be recovered before applying another")
	}
	rec := pairRecord{BinaryPath: binary, Phase: "prepared"}
	generation, err := os.CreateTemp(filepath.Dir(binary), ".pair-generation-*")
	if err != nil {
		return err
	}
	generation.Close()
	os.Remove(generation.Name())
	suffix := filepath.Base(generation.Name())
	published := false
	defer func() {
		if !published {
			for _, m := range rec.Members {
				_ = os.Remove(m.PrevPath)
			}
		}
	}()
	staged := map[string]string{}
	for _, name := range ProductMembers() {
		src := companions[name]
		target := filepath.Join(filepath.Dir(binary), name)
		if name == productName("outpost") {
			src = candidate
			target = binary
		}
		if src == "" {
			continue
		}
		fi, err := os.Stat(src)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("invalid staged member %s", name)
		}
		m := RollbackMember{BinaryPath: target, PrevPath: target + ".previous-" + suffix}
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			m.Absent = true
		} else if err != nil {
			return err
		} else if err := RetainPrevious(target, m.PrevPath); err != nil {
			return fmt.Errorf("retain %s: %w", name, err)
		}
		if !m.Absent {
			sum, err := pairFileHash(m.PrevPath)
			if err != nil {
				return err
			}
			m.SHA256 = sum
		}
		rec.Members = append(rec.Members, m)
		staged[target] = src
	}
	if err := writePairRecord(rec); err != nil {
		if persisted, _ := readPairRecord(binary); persisted != nil && len(persisted.Members) > 0 && persisted.Members[0].PrevPath == rec.Members[0].PrevPath {
			published = true
			return errors.Join(err, restorePair(rec))
		}
		return err
	}
	published = true
	undo := func() error {
		if err := restorePair(rec); err != nil {
			return err
		}
		if prior != nil {
			return writePairRecord(*prior)
		}
		return nil
	}
	if beforeSwap != nil {
		if err := beforeSwap(); err != nil {
			return errors.Join(err, undo())
		}
	}
	for _, m := range rec.Members {
		if err := SwapAtomic(m.BinaryPath, staged[m.BinaryPath]); err != nil {
			undoErr := undo()
			return errors.Join(fmt.Errorf("swap %s: %w", m.BinaryPath, err), undoErr)
		}
	}
	rec.Phase = "complete"
	if err := writePairRecord(rec); err != nil {
		return errors.Join(err, undo())
	}
	if prior != nil {
		for _, m := range prior.Members {
			_ = os.Remove(m.PrevPath)
		}
	}
	return nil
}

// restorePair copies retained bytes through a staged file, keeping the rollback
// source intact until ALL restores succeed, so interrupted recovery is retryable.
func restorePair(rec pairRecord) error {
	// Validate every retained source before restoring ANY member.
	for _, m := range rec.Members {
		if m.Absent {
			continue
		}
		sum, err := pairFileHash(m.PrevPath)
		if err != nil {
			return err
		}
		if m.SHA256 == "" || sum != m.SHA256 {
			return fmt.Errorf("rollback checksum mismatch for %s", m.BinaryPath)
		}
	}
	if rec.Phase != "prepared" {
		rec.Phase = "prepared"
		if err := writePairRecord(rec); err != nil {
			return err
		}
	}

	for _, m := range rec.Members {
		if m.Absent {
			if err := os.Remove(m.BinaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		tmp := m.BinaryPath + ".reverting"
		_ = os.Remove(tmp)
		if err := StageFromLocal(m.PrevPath, tmp, ""); err != nil {
			return err
		}
		if err := SwapAtomic(m.BinaryPath, tmp); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Remove(pairRecordPath(rec.BinaryPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, m := range rec.Members {
		_ = os.Remove(m.PrevPath)
	}
	return nil
}

// RecoverInterruptedPair restores a prepared transaction before the supervisor
// launches a daemon. This is crash recovery, independent of health rollback policy.
func RecoverInterruptedPair(binary string) (bool, error) {
	rec, err := readPairRecord(binary)
	if err != nil {
		return false, err
	}
	if rec == nil || rec.Phase != "prepared" {
		return false, nil
	}
	if err := restorePair(*rec); err != nil {
		return false, err
	}
	return true, nil
}

func PairRollbackMembers(binary string) []RollbackMember {
	rec, err := readPairRecord(binary)
	if err != nil || rec == nil {
		return nil
	}
	return rec.Members
}

func pairFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular executable: %s", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func IsPairedReleaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host == "github.com" && strings.HasPrefix(u.Path, "/qiangli/bashy/releases/download/")
}
