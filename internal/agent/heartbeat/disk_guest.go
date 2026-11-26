package heartbeat

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Only fixed record types, filesystem numbers and byte counts reach stdout.
// Include the root and every filesystem containing a monitored store. Ignore
// incomplete du results. Each du is bounded even if a WSL client disconnects.
const diskScript = `
export LC_ALL=C
volume() {
 stat -f -c 'v %i %S %b %f' -- "$1"
}
store() {
 label=$1
 shift
 [ "$#" -gt 0 ] || return
 for p do
  [ -e "$p" ] || continue
  if [ -L "$p" ]; then
   case "$label" in go-mod|go-build) p=$(readlink -f -- "$p") || continue ;; *) continue ;; esac
  fi
  volume "$p" || continue
  fs=$(stat -f -c %i -- "$p") || continue
  n=$(timeout 20 du -s -x -B1 -- "$p" 2>/dev/null) || { printf 'u %s %s 0\n' "$fs" "$label"; continue; }
  printf 's %s %s %s\n' "$fs" "$label" "${n%%[[:space:]]*}"
 done
 printf 'end %s\n' "$label"
}
stat -f -c 'r %i %S %b %f' /
# Include other local data filesystems even when none of the known stores
# live on them. /proc encodes whitespace and backslashes in mount points.
if [ -r /proc/mounts ]; then
 count=0
 while read -r dev mount type rest; do
  case "$type" in ext[234]|xfs|btrfs|f2fs|zfs|vfat|exfat|ntfs3)
   [ "$mount" = / ] && continue
   volume "$(printf '%b' "$mount")"
   count=$((count+1)); [ "$count" -lt 32 ] || break ;;
  esac
 done < /proc/mounts
fi
# Honor settings persisted by 'go env -w' as well as job environment
# overrides. Read only the three path settings; never source the file.
goenv=${GOENV:-"${XDG_CONFIG_HOME:-$HOME/.config}/go/env"}
if [ "$goenv" != off ] && [ -f "$goenv" ] && [ -r "$goenv" ]; then
 while IFS= read -r setting; do
  case "$setting" in
   GOCACHE=*) GOCACHE=${GOCACHE:-${setting#*=}} ;;
   GOMODCACHE=*) GOMODCACHE=${GOMODCACHE:-${setting#*=}} ;;
   GOPATH=*) GOPATH=${GOPATH:-${setting#*=}} ;;
  esac
 done < "$goenv"
fi
cache=${XDG_CACHE_HOME:-"$HOME/.cache"}/outpost
store logs "$cache/daemon.log.1" "$cache/bashy-supervisord.log.1" "$cache/nodegc.log" "$cache/daemon.log" "$cache/bashy-supervisord.log" "$cache/supervisord.log" "$cache/outpost.log" "$cache/cli-trace.log" "$cache/upgrade.log" "$cache/scheduler.log"
store evidence "$cache/post-install.json" "$cache/post-install-reboot.json" "$cache/post-install-reboot.pending.json"
if [ -n "$BASHY_OTEL_SPOOL" ]; then store otel "$BASHY_OTEL_SPOOL"; else store otel "$HOME/.agents/otel/spool"; fi
store go-build "${GOCACHE:-${XDG_CACHE_HOME:-"$HOME/.cache"}/go-build}"
gp=${GOPATH:-"$HOME/go"}
store go-mod "${GOMODCACHE:-${gp%%:*}/pkg/mod}"
exit 0
`

func linuxMeasurements(ctx context.Context, kind, distro, display, program string, args ...string) []measurement {
	raw, err := probeOutput(ctx, program, args...)
	if err != nil && ctx.Err() == nil {
		return nil
	}
	return parseDiskRows(kind, distro, display, string(raw))
}

type probeBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *probeBuffer) Write(p []byte) (int, error) {
	const limit = 16 << 10
	n := min(len(p), limit-b.buf.Len())
	b.buf.Write(p[:n])
	if n < len(p) {
		b.overflow = true
	}
	return len(p), nil
}
func probeOutput(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = time.Second
	var b probeBuffer
	cmd.Stdout = &b
	err := cmd.Run()
	if b.overflow {
		return nil, fmt.Errorf("disk probe exceeded output limit")
	}
	return b.buf.Bytes(), err
}

// WSL list output can be UTF-16LE even when captured through a pipe.
func runningDistros(raw []byte) map[string]bool {
	if len(raw) >= 2 && (raw[1] == 0 || binary.LittleEndian.Uint16(raw) == 0xfeff) {
		var words []uint16
		for len(raw) >= 2 {
			words = append(words, binary.LittleEndian.Uint16(raw))
			raw = raw[2:]
		}
		raw = []byte(string(utf16.Decode(words)))
	}
	result := map[string]bool{}
	for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\uFEFF"), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			result[strings.ToLower(name)] = true
		}
	}
	return result
}
func validLabel(s string) bool {
	switch s {
	case "logs", "evidence", "otel", "go-build", "go-mod":
		return true
	}
	return false
}

var distroLabel = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]{0,47}$")

func parseDiskRows(kind, distro, display, raw string) []measurement {
	volumes := map[string]measurement{}
	// A timed-out directory scan must not turn a partial byte count into a
	// lower baseline. Only completed label groups contribute sizes.
	complete := map[string]bool{}
	failed := map[[2]string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "end" && validLabel(f[1]) {
			complete[f[1]] = true
			continue
		}
		if len(f) < 4 {
			continue
		}
		if _, err := strconv.ParseUint(f[1], 16, 64); err != nil {
			continue
		}
		id := volumeID(kind, distro+"/"+f[1])
		switch f[0] {
		case "v", "r":
			if len(f) != 5 || len(volumes) >= maxVolumes {
				continue
			}
			if _, ok := volumes[id]; ok {
				continue
			}
			bs, e := strconv.ParseUint(f[2], 10, 64)
			blocks, b := strconv.ParseUint(f[3], 10, 64)
			free, c := strconv.ParseUint(f[4], 10, 64)
			if e != nil || b != nil || c != nil || bs == 0 || blocks == 0 || free > blocks || blocks > math.MaxUint64/bs {
				continue
			}
			name := display + " fs " + f[1]
			if f[0] == "r" {
				name = display + " /"
			}
			volumes[id] = measurement{diskVolume: diskVolume{DisplayName: name, ID: id, Used: (blocks - free) * bs, Total: blocks * bs}, sizes: map[string]uint64{}}
		case "u":
			if validLabel(f[2]) {
				failed[[2]string{id, f[2]}] = true
			}
		case "s":
			v, ok := volumes[id]
			size, e := strconv.ParseUint(f[3], 10, 64)
			if ok && len(f) == 4 && validLabel(f[2]) && e == nil && size <= math.MaxUint64-v.sizes[f[2]] {
				v.sizes[f[2]] += size
			}
		}
	}
	result := make([]measurement, 0, len(volumes))
	for _, v := range volumes {
		for label := range v.sizes {
			if !complete[label] || failed[[2]string{v.ID, label}] {
				delete(v.sizes, label)
			}
		}
		result = append(result, v)
	}
	return result
}
