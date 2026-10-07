// Package bashypath locates the local userland without downloading or starting it.
package bashypath

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

var ErrNotFound = errors.New("bashy executable missing")

func Name() string {
	if runtime.GOOS == "windows" {
		return "bashy.exe"
	}
	return "bashy"
}

// Sibling follows installation links before finding the paired executable.
func Sibling(executable func() (string, error)) string {
	if executable == nil {
		executable = os.Executable
	}
	exe, err := executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), Name())
}

func Executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0)
}

// Candidates are the stable user and managed cache locations used by services
// whose inherited PATH may omit the user's install directory.
func Candidates() []string {
	var dirs []string
	if sibling := Sibling(nil); sibling != "" {
		dirs = append(dirs, filepath.Dir(sibling))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "bin"), filepath.Join(home, ".local", "bin"))
	}
	dirs = append(dirs, "/usr/local/bin", "/opt/homebrew/bin")
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(cache, "outpost", "bin"))
	}
	paths := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		paths = append(paths, filepath.Join(dir, Name()))
	}
	return paths
}

func Managed(path string, executable func() (string, error)) bool {
	dir := filepath.Clean(filepath.Dir(path))
	if sibling := Sibling(executable); sibling != "" && dir == filepath.Clean(filepath.Dir(sibling)) {
		return true
	}
	if cache, err := os.UserCacheDir(); err == nil && dir == filepath.Join(cache, "outpost", "bin") {
		return true
	}
	return false
}

// Find prefers the installed pair, then explicit override, PATH, and familiar
// install locations. The boolean identifies paths eligible for managed updates.
// A shell request never downloads a missing binary.
func Find(executable func() (string, error)) (string, bool, error) {
	if p := Sibling(executable); Executable(p) {
		return p, true, nil
	}
	if p := strings.TrimSpace(os.Getenv("OUTPOST_BASHY_BIN")); p != "" {
		if !Executable(p) {
			return "", false, fmt.Errorf("OUTPOST_BASHY_BIN=%q is not executable", p)
		}
		p, err := filepath.Abs(p)
		return p, false, err
	}
	if p, err := exec.LookPath(Name()); err == nil {
		p, err = filepath.Abs(p)
		return p, Managed(p, executable), err
	}
	for _, p := range Candidates() {
		if Executable(p) {
			return p, Managed(p, executable), nil
		}
	}
	// Versioned binmgr cache entries are immutable; do not reconcile them in place.
	if p := binmgr.CachedBinary("bashy"); p != "" && Executable(p) {
		return p, false, nil
	}
	return "", false, fmt.Errorf("%w: install the paired bashy beside outpost or set OUTPOST_BASHY_BIN to a local executable", ErrNotFound)
}
