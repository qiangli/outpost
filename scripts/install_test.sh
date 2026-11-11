#!/bin/sh
# install.sh defaults to NO service (outpost dormant until `bashy login`).
# A service scope without service installation is rejected before any
# download, so the rejection is the observable proof of the default.

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
installer="$script_dir/install.sh"

expect_scope_rejected() {
	if out=$("$@" --user 2>&1); then
		echo "expected rejection: $*" >&2
		exit 1
	fi
	case "$out" in
		*'service scope requires service installation'*) ;;
		*) echo "unexpected output for $*: $out" >&2; exit 1 ;;
	esac
}

expect_scope_rejected "$installer"
expect_scope_rejected "$installer" --no-service
expect_scope_rejected "$installer" --service --no-service
expect_scope_rejected env SERVICE=1 "$installer" --no-service
expect_scope_rejected env SERVICE=1 NO_SERVICE=1 "$installer"
expect_scope_rejected env NO_SERVICE=1 "$installer"

echo "install: all tests passed"
