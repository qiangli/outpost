#!/bin/sh
#
# Exit successfully when two Git object IDs identify the same commit.
# Git commonly crosses API boundaries as a mixture of full object IDs and
# abbreviated IDs, so compare by prefix after validating both values. Seven
# hexadecimal characters is the minimum accepted abbreviation: short enough
# for outpost's BuildInfo report, but long enough to reject ambiguous scraps.

set -eu

usage() {
	echo "usage: $0 <git-sha> <git-sha>" >&2
	exit 2
}

normalize_sha() {
	sha=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
	case "$sha" in
	"" | *[!0-9a-f]*)
		return 1
		;;
	esac
	[ "${#sha}" -ge 7 ] && [ "${#sha}" -le 64 ] || return 1
	printf '%s\n' "$sha"
}

[ "$#" -eq 2 ] || usage

left=$(normalize_sha "$1") || exit 1
right=$(normalize_sha "$2") || exit 1

case "$left" in
"$right"*)
	exit 0
	;;
esac
case "$right" in
"$left"*)
	exit 0
	;;
esac
exit 1
