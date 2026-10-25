package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// bootpath.go — one PATH contract on every OS.
//
// A boot session (launchd daemon, systemd unit, Task Scheduler S4U task)
// inherits a minimal PATH that excludes every package-manager prefix, so
// a Homebrew- or bashy-installed `podman` is invisible to the daemon
// even though it resolves fine in a login shell. The service
// definitions pin PATH where the OS allows it (launchd plist,
// systemd unit); where it doesn't (Windows tasks carry no environment)
// and as a backstop everywhere else, the supervisor and the daemon
// heal their own PATH at boot via ensureBootPATH before launching
// anything. `outpost verify --post-install` probes the same expected
// prefixes through bootPathSane, so all three platforms run the same
// check against the same lists.

// bootPathPrefixes lists the PATH entries a boot session must carry on
// the given OS. goos is a parameter (not runtime.GOOS directly) so
// tests pin every platform's list on any host.
// winJoin joins Windows path elements with backslashes regardless of
// the host doing the test: filepath.Join follows the HOST OS, which
// would emit forward slashes when unit tests pin the Windows list on
// macOS/Linux.
func winJoin(elem ...string) string {
	return strings.Join(elem, `\`)
}

func bootPathPrefixes(goos string) []string {
	if goos == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return []string{
			winJoin(root, "System32"),
			root,
			winJoin(root, "System32", "WindowsPowerShell", "v1.0"),
		}
	}
	base := []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if goos == "darwin" {
		return append(base, "/opt/homebrew/bin")
	}
	return base
}

// missingPathEntries reports which of want are absent from a
// PATH-style list. Comparison is case-insensitive on Windows
// (System32 vs SYSTEM32) and exact elsewhere.
func missingPathEntries(pathEnv string, want []string, foldCase bool) []string {
	// Windows PATH entries carry drive-letter colons (`C:\...`), so a
	// Windows list always splits on `;` — even when this check runs on
	// a non-Windows host (unit tests pin the Windows list on macOS).
	sep := string(os.PathListSeparator)
	if foldCase {
		sep = ";"
	}
	have := strings.Split(pathEnv, sep)
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if foldCase {
			// Windows: normalize separators and case so
			// `C:\Windows/System32` still matches `C:\Windows\System32`
			// even when the check runs on a non-Windows host.
			s = strings.ReplaceAll(s, "/", `\`)
			s = strings.TrimRight(s, `\`)
			return strings.ToLower(s)
		}
		return filepath.Clean(s)
	}
	set := map[string]bool{}
	for _, h := range have {
		if h = clean(h); h != "" && h != "." {
			set[h] = true
		}
	}
	var missing []string
	for _, w := range want {
		if !set[clean(w)] {
			missing = append(missing, w)
		}
	}
	return missing
}

// bootPathSane reports whether the current process PATH carries every
// expected prefix for this OS, plus a human detail naming what's
// missing. `outpost verify --post-install` runs this on all three
// platforms; the supervisor's ensureBootPATH is what makes a bare
// boot session pass it.
func bootPathSane() (bool, string) {
	goos := runtime.GOOS
	want := bootPathPrefixes(goos)
	missing := missingPathEntries(os.Getenv("PATH"), want, goos == "windows")
	if len(missing) == 0 {
		return true, ""
	}
	return false, "PATH missing " + strings.Join(missing, string(os.PathListSeparator))
}

// ensureBootPATH prepends any missing expected prefix to this
// process's PATH (deduped — already-present entries are left alone).
// Called at supervisor and daemon boot so children (podman lookups,
// unit processes) inherit a sane PATH even when the OS service hands
// us a bare one. Logs what it adds; never removes anything.
func ensureBootPATH() {
	goos := runtime.GOOS
	want := bootPathPrefixes(goos)
	missing := missingPathEntries(os.Getenv("PATH"), want, goos == "windows")
	if len(missing) == 0 {
		return
	}
	sep := string(os.PathListSeparator)
	os.Setenv("PATH", strings.Join(missing, sep)+sep+os.Getenv("PATH"))
	slog.Info("boot PATH healed", "added", missing)
}
