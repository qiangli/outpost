//go:build linux

package osversion

import (
	"bufio"
	"os"
	"strings"
)

// detect on linux reads /etc/os-release (systemd-blessed standard
// across distros). Returns PRETTY_NAME when present (e.g. "Ubuntu
// 24.04.1 LTS"); falls back to NAME + VERSION_ID; empty if neither
// the file nor the keys exist (Alpine before some versions, embedded
// systems without /etc/os-release).
func detect() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	kv := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		k := line[:idx]
		v := strings.Trim(line[idx+1:], `"'`)
		kv[k] = v
	}
	if pretty := kv["PRETTY_NAME"]; pretty != "" {
		return pretty
	}
	name := kv["NAME"]
	ver := kv["VERSION_ID"]
	if name == "" {
		return ""
	}
	if ver == "" {
		return name
	}
	return name + " " + ver
}
