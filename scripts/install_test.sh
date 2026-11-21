#!/bin/sh
# Verify installer defaults and service scope without downloads or OS changes.

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

fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT HUP INT TERM
mkdir "$fixture/bin" "$fixture/payload" "$fixture/install dir"
cat >"$fixture/bin/curl" <<'EOF'
#!/bin/sh
cp "$FIXTURE/${2##*/}" "$4"
EOF
cat >"$fixture/bin/id" <<'EOF'
#!/bin/sh
echo 1000
EOF
cat >"$fixture/bin/sudo" <<'EOF'
#!/bin/sh
printf 'sudo <%s> <%s> <%s> <%s> <%s>\n' "$@" >>"$FIXTURE/calls"
exit "${SUDO_RESULT:-0}"
EOF
cat >"$fixture/payload/bashy" <<'EOF'
#!/bin/sh
printf 'bashy' >>"$FIXTURE/calls"
printf ' <%s>' "$@" >>"$FIXTURE/calls"
printf '\n' >>"$FIXTURE/calls"
EOF
chmod +x "$fixture/bin/"* "$fixture/payload/bashy"
touch "$fixture/payload/outpost" "$fixture/payload/bash" "$fixture/payload/sh"
case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; esac
case $(uname -m) in arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
asset=bashy-$os-$arch.tar.gz
tar -czf "$fixture/$asset" -C "$fixture/payload" bashy outpost bash sh
checksum=$(if command -v sha256sum >/dev/null; then sha256sum "$fixture/$asset"; else shasum -a 256 "$fixture/$asset"; fi)
printf '%s  %s\n' "${checksum%% *}" "$asset" >"$fixture/checksums.txt"

run_install() {
    : >"$fixture/calls"
    FIXTURE="$fixture" PATH="$fixture/bin:$PATH" SERVICE= NO_SERVICE= \
        "$installer" --version fixture --dir "$fixture/install dir" "$@" >/dev/null
}

install_call="bashy <self> <install> <--dir> <$fixture/install dir>"
installed_dir=$(CDPATH= cd -- "$fixture/install dir" && pwd -P)
boot_call="sudo <--> <$installed_dir/outpost> <service> <install> <--system>"
run_install
[ "$(cat "$fixture/calls")" = "$install_call" ]
for scope in default system; do
    set -- --service
    if [ "$scope" = system ]; then set -- "$@" --system; fi
    run_install "$@"
    [ "$(cat "$fixture/calls")" = "$(printf '%s\n' "$install_call" "$boot_call")" ]
done
run_install --service --user
[ "$(cat "$fixture/calls")" = "$install_call <--service> <--user>" ]
if SUDO_RESULT=1 run_install --service; then
    echo 'expected failed boot registration to fail the installer' >&2
    exit 1
fi

echo "install: all tests passed"
