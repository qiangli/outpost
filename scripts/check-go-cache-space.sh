#!/bin/sh
# Read-only admission check for a WSL job that will populate persistent Go caches.
# Set GO_CACHE_VOLUME_MAX_BYTES to also verify a provisioned filesystem ceiling.
set -eu
export LC_ALL=C
build=${GOCACHE:-}
modules=${GOMODCACHE:-}
if [ -z "$build" ]; then build=$(GOTOOLCHAIN=local GOPROXY=off go env GOCACHE); fi
if [ -z "$modules" ]; then modules=$(GOTOOLCHAIN=local GOPROXY=off go env GOMODCACHE); fi
ceiling=${GO_CACHE_VOLUME_MAX_BYTES:-0}
case "$ceiling" in ''|*[!0-9]*) echo "invalid cache volume byte ceiling" >&2; exit 2 ;; esac

check_volume() {
 label=$1
 path=$2
 case "$path" in /*) ;; *) echo "$label requires an absolute cache directory" >&2; return 2 ;; esac
 # Check from the actual job namespace. Never create or clean the cache.
 while [ ! -d "$path" ]; do
  if [ -e "$path" ] || [ -L "$path" ]; then echo "$label is not a cache directory" >&2; return 2; fi
  path=$(dirname -- "$path")
 done
 data=$(stat -f -c '%S %b %a' -- "$path") || return 2
 printf '%s\n' "$data" | awk -v label="$label" -v ceiling="$ceiling" '
  NF != 3 || $1 !~ /^[0-9]+$/ || $2 !~ /^[0-9]+$/ || $3 !~ /^[0-9]+$/ || $1 < 1 || $2 < 1 || $3 > $2 { print label ": invalid filesystem statistics"; exit 2 }
  {
   total = $1 * $2
   printf "%s total_bytes=%.0f available_bytes=%.0f\n", label, total, $1 * $3
   if (ceiling > 0 && total > ceiling) { print label ": backing volume exceeds configured ceiling"; exit 1 }
   if ($3 * 100 <= $2 * 5) { print label ": at or above 95% usage; defer new cache writes"; exit 1 }
  }
  END { if (NR != 1) { print label ": missing or ambiguous filesystem statistics"; exit 2 } }'
}
check_volume go-mod "$modules"
check_volume go-build "$build"
