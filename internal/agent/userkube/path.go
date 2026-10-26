package userkube

import (
	"runtime"
	"strings"
)

// NormalizePath converts an MSYS or WSL drive path (/c/... -> C:\...) to
// a native Windows path on Windows. On non-Windows platforms or for paths that
// do not match a drive pattern, it returns the input unchanged.
func NormalizePath(p string) string {
	return normalizePathOS(p, runtime.GOOS)
}

func normalizePathOS(p, goos string) string {
	if goos != "windows" {
		return p
	}
	if len(p) < 2 {
		return p
	}
	// MSYS drive path: /c/... or /c
	if p[0] == '/' {
		drive := p[1]
		if (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z') {
			if len(p) == 2 {
				return strings.ToUpper(string(drive)) + `:\`
			}
			if p[2] == '/' || p[2] == '\\' {
				rest := strings.ReplaceAll(p[2:], "/", `\`)
				return strings.ToUpper(string(drive)) + ":" + rest
			}
		}
	}
	// WSL drive path: /mnt/c/... or /mnt/c
	if strings.HasPrefix(p, "/mnt/") && len(p) >= 6 {
		drive := p[5]
		if (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z') {
			if len(p) == 6 {
				return strings.ToUpper(string(drive)) + `:\`
			}
			if p[6] == '/' || p[6] == '\\' {
				rest := strings.ReplaceAll(p[6:], "/", `\`)
				return strings.ToUpper(string(drive)) + ":" + rest
			}
		}
	}
	return p
}
