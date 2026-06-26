// Copyright (c) 2026, the outpost authors
// See LICENSE for licensing information

package shell

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"mvdan.cc/sh/v3/expand"
)

// BuildEnv returns the env that the in-process matrix shell should run in.
//
// Starts from the outpost daemon's own process env (os.Environ()) and
// **prepends** to PATH a small fixed set of "user-shell-style" directories
// that launchd-spawned daemons consistently lack:
//
//   - the directory containing the running outpost binary itself
//     (without this, `$(which outpost)` returns empty inside the shell —
//     hits any agentic flow that does `ls -la $(which outpost)` style
//     introspection)
//   - $HOME/bin and $HOME/.local/bin (the standard places a user puts
//     locally-installed binaries)
//   - /opt/homebrew/{bin,sbin} (macOS Homebrew on Apple Silicon — usually
//     in launchd's default PATH but only on newer macOS versions)
//   - /usr/local/bin and /usr/local/sbin (Intel Homebrew, MacPorts;
//     common deploy target for `make install`)
//
// On Windows, service-spawned sessions commonly inherit only the outpost
// directory in PATH, so BuildEnv also adds the standard Windows executable
// directories such as C:\Windows\System32 when they are missing.
//
// Entries that don't exist or that PATH already contains are skipped, so
// running this on a host with a fully-correct PATH is a no-op. Dedup is
// case-insensitive on Windows to match PATH semantics there.
//
// Returns an expand.Environ suitable for passing to interp.Env(...).
func BuildEnv() expand.Environ {
	return BuildEnvWith(nil)
}

// BuildEnvWith is BuildEnv with caller-supplied overrides applied on top.
// Each key in overrides replaces any existing entry of that name in the
// outpost process env; absent keys are appended. Used by NewSession to
// stamp TERM (from the SSH client's pty-req) so vim/htop/less know what
// escape sequences the terminal understands. Pass nil for no overrides
// (equivalent to BuildEnv).
func BuildEnvWith(overrides map[string]string) expand.Environ {
	env := os.Environ()

	// Locate (or stub in) the PATH= entry. There's one in 99% of cases;
	// the 1% where launchd doesn't propagate one is precisely the kind
	// of environment this helper is built for.
	pathIdx := -1
	pathKey := "PATH"
	var paths []string
	for i, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && envKeyEqual(k, "PATH", runtime.GOOS) {
			pathIdx = i
			pathKey = k
			paths = strings.Split(v, string(os.PathListSeparator))
			break
		}
	}

	extras := []string{}
	if exe, err := os.Executable(); err == nil {
		extras = append(extras, filepath.Dir(exe))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		extras = append(extras,
			filepath.Join(home, "bin"),
			filepath.Join(home, ".local", "bin"),
		)
	}
	extras = append(extras,
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
	)
	extras = append(extras, windowsPathExtras(env, runtime.GOOS)...)

	paths = augmentPathEntries(paths, extras, runtime.GOOS, dirExists)

	newPATH := pathKey + "=" + strings.Join(paths, string(os.PathListSeparator))
	if pathIdx >= 0 {
		env[pathIdx] = newPATH
	} else {
		env = append(env, newPATH)
	}

	for k, v := range overrides {
		kv := k + "=" + v
		prefix := k + "="
		replaced := false
		for i, existing := range env {
			if strings.HasPrefix(existing, prefix) {
				env[i] = kv
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, kv)
		}
	}
	return expand.ListEnviron(env...)
}

func augmentPathEntries(paths, extras []string, goos string, exists func(string) bool) []string {
	seen := make(map[string]bool, len(paths)+len(extras))
	for _, p := range paths {
		seen[pathSeenKey(p, goos)] = true
	}
	var prepend []string
	for _, p := range extras {
		if p == "" || seen[pathSeenKey(p, goos)] {
			continue
		}
		// Don't pollute PATH with dirs that don't exist on this host.
		if !exists(p) {
			continue
		}
		seen[pathSeenKey(p, goos)] = true
		prepend = append(prepend, p)
	}
	if len(prepend) == 0 {
		return paths
	}
	out := make([]string, 0, len(prepend)+len(paths))
	out = append(out, prepend...)
	out = append(out, paths...)
	return out
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func windowsPathExtras(env []string, goos string) []string {
	if goos != "windows" {
		return nil
	}
	windir := envValue(env, "SystemRoot", goos)
	if windir == "" {
		windir = envValue(env, "WINDIR", goos)
	}
	if windir == "" {
		windir = `C:\Windows`
	}
	return []string{
		winPathJoin(windir, "System32"),
		windir,
		winPathJoin(windir, "System32", "Wbem"),
		winPathJoin(windir, "System32", "WindowsPowerShell", "v1.0"),
		winPathJoin(windir, "System32", "OpenSSH"),
		`C:\Program Files\NVIDIA Corporation\NVSMI`,
	}
}

func envValue(env []string, key, goos string) string {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && envKeyEqual(k, key, goos) {
			return v
		}
	}
	return ""
}

func envKeyEqual(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func pathSeenKey(p, goos string) string {
	if goos == "windows" {
		return strings.ToLower(p)
	}
	return p
}

func winPathJoin(base string, parts ...string) string {
	out := strings.TrimRight(base, `\/`)
	for _, p := range parts {
		p = strings.Trim(p, `\/`)
		if p == "" {
			continue
		}
		out += `\` + p
	}
	return out
}
