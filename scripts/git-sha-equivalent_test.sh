#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
matcher="$script_dir/git-sha-equivalent.sh"

expect_match() {
	if ! "$matcher" "$1" "$2"; then
		echo "expected equivalent: $1 $2" >&2
		exit 1
	fi
}

expect_no_match() {
	if "$matcher" "$1" "$2"; then
		echo "expected different or invalid: $1 $2" >&2
		exit 1
	fi
}

# The rollout regression: BuildInfo currently reports seven characters while
# the release envelope carries a full GitHub SHA.
expect_match d5cd684 d5cd6841234567890abcdef1234567890abcdef
expect_match d5cd68412345 d5cd6841234567890abcdef1234567890abcdef
expect_match d5cd6841234567890abcdef1234567890abcdef d5cd684
expect_match D5CD684 d5cd6841234567890abcdef1234567890abcdef

expect_no_match d5cd684 e5cd6841234567890abcdef1234567890abcdef
expect_no_match d5cd68 d5cd6841234567890abcdef1234567890abcdef
expect_no_match "" d5cd6841234567890abcdef1234567890abcdef
expect_no_match null d5cd6841234567890abcdef1234567890abcdef
expect_no_match d5cd684- d5cd6841234567890abcdef1234567890abcdef

echo "git-sha-equivalent: all tests passed"
