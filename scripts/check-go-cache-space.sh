#!/bin/sh
# Verify the dedicated, size-bounded filesystem used by WSL Go caches.
set -eu
export LC_ALL=C

fail() { printf '%s\n' "$*" >&2; exit 2; }
ceiling=${GO_CACHE_VOLUME_MAX_BYTES:-}
volume=${GO_CACHE_VOLUME_ROOT:-}
case "$ceiling" in ''|*[!0-9]*) fail "configure GO_CACHE_VOLUME_MAX_BYTES with the cache filesystem byte ceiling" ;; esac
awk -v ceiling="$ceiling" 'BEGIN { exit !(ceiling > 0 && ceiling <= 9007199254740991) }' ||
    fail "GO_CACHE_VOLUME_MAX_BYTES must be a positive, exact byte count"
case "$volume" in /*) ;; *) fail "configure GO_CACHE_VOLUME_ROOT with the dedicated cache mount" ;; esac

volume=$(realpath -e -- "$volume") || fail "cache mount does not exist"
if [ "$volume" = / ] || ! mountpoint -q -- "$volume" ||
   [ "$(findmnt --noheadings --output FSROOT --target "$volume")" != / ]; then
    fail "cache volume must be mounted as a separate filesystem"
fi
device=$(stat -c '%d' -- "$volume")
if [ "$device" = "$(stat -c '%d' -- /)" ] ||
   [ "$device" = "$(stat -c '%d' -- "$(dirname -- "$volume")")" ]; then
    fail "cache volume shares a filesystem with its parent or the system root"
fi

check_cache_path() {
    label=$1
    path=$2
    case "$path" in /*) ;; *) fail "$label requires an absolute cache directory" ;; esac
    path=$(realpath -m -- "$path") || fail "$label path cannot be resolved"
    case "$path" in "$volume"/*) ;; *) fail "$label must be inside the dedicated cache mount" ;; esac
    while [ ! -d "$path" ]; do
        if [ -e "$path" ] || [ -L "$path" ]; then fail "$label is not a cache directory"; fi
        path=$(dirname -- "$path")
    done
    [ "$(stat -c '%d' -- "$path")" = "$device" ] || fail "$label uses another filesystem"
}

build=${GOCACHE:-}
modules=${GOMODCACHE:-}
if [ -z "$build" ]; then build=$(GOTOOLCHAIN=local GOPROXY=off go env GOCACHE); fi
if [ -z "$modules" ]; then modules=$(GOTOOLCHAIN=local GOPROXY=off go env GOMODCACHE); fi
check_cache_path go-mod "$modules"
check_cache_path go-build "$build"

data=$(stat -f -c '%S %b %a' -- "$volume")
printf '%s\n' "$data" | awk -v ceiling="$ceiling" '
    NF != 3 || $1 !~ /^[0-9]+$/ || $2 !~ /^[0-9]+$/ || $3 !~ /^[0-9]+$/ || $1 < 1 || $2 < 1 || $3 > $2 {
        print "cache volume: invalid filesystem statistics"; exit 2
    }
    {
        total = $1 * $2
        printf "cache volume total_bytes=%.0f available_bytes=%.0f\n", total, $1 * $3
        if (total > ceiling) { print "cache volume exceeds configured ceiling"; exit 1 }
        if ($3 * 100 <= $2 * 5) { print "cache volume at or above 95% usage; defer new cache writes"; exit 1 }
    }
    END { if (NR != 1) { print "cache volume: missing or ambiguous filesystem statistics"; exit 2 } }'
