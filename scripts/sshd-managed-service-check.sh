#!/usr/bin/env bash
#
# sshd-managed-service-check.sh — O4 managed-service sshd proof (Story 1537).
#
# Given a bashy+outpost pair (one archive per OS, matched pair), this script
# proves outpost serves sshd as a managed service installed by bashy:
#
#   1. installs the pair and registers the managed service with
#      `bashy self install --service` (the Sprint 386 boundary) —
#      per-user by default (launchd user agent on macOS, systemd --user
#      on Linux, logon task on Windows), or a boot-time service running
#      as the current user with O4_MODE=system;
#   2. points the managed daemon at a dedicated ssh_listen_addr port;
#   3. logs in over stock ssh with the documented authorized_keys auth,
#      runs `echo o4-ok`, and does an SFTP put/get round trip;
#   4. stops and uninstalls the service, leaving nothing running.
#
# Isolation: everything the check owns lives under a mktemp dir. $HOME,
# $XDG_CONFIG_HOME, $XDG_CACHE_HOME and $BASHY_HOME all point there for
# every direct invocation, and the script propagates XDG_*/OUTPOST_ADMIN_ADDR
# into the service manager context where the manager allows it
# (systemd --user honors XDG_CONFIG_HOME for the unit path;
# `systemctl --user import-environment` / `launchctl setenv` carry the
# daemon env). The one exception is the daemon's authorized_keys lookup,
# which always resolves to the service user's real ~/.ssh/authorized_keys:
# the script appends one ephemeral key there (backed up first, restored on
# exit). Removal takes effect immediately server-side (the server reloads
# the file per authentication), so cleanup is safe.
#
# The daemon binds ssh_listen_addr on LOOPBACK (127.0.0.1) only: this proves
# the managed listener end to end without exposing a test sshd to the LAN.
# Binding all interfaces is a deployment-time address choice, not a
# different code path (same ServeLANSSH handler).
#
# Re-run:   BASHY_BIN=/path/to/product/bashy ./scripts/sshd-managed-service-check.sh
# Windows:  see scripts/sshd-managed-service-check.ps1 (same steps; schtasks
#           needs PowerShell, and ssh/sftp ship with Windows 10+).
#
# Env knobs (all optional):
#   BASHY_BIN          candidate bashy; its directory must also hold the
#                      matched bash, sh and outpost (the 4-file product).
#                      Default: first `bashy` on PATH.
#   O4_PRODUCT_DIR     override the product dir (default: dirname of BASHY_BIN).
#   O4_MODE            user (default) or system. system needs admin (sudo /
#                      elevated prompt) and registers a boot-time service
#                      running as the current user.
#   O4_PORT            dedicated sshd port. Default 22022.
#   O4_ADMIN_PORT      isolated daemon admin port. Default 17779.
#   O4_TIMEOUT_SECS    bound for each wait (daemon start, port close, ...).
#                      Default 120.
#   O4_SSH_TIMEOUT     per-ssh-command timeout (ConnectTimeout). Default 15.
#   O4_KEEPDIR=1       keep the temp dir on exit (debugging).
#
# Exit codes: 0 PASS; 1 FAIL; 2 usage/config error; 3 SKIP (service manager
# unavailable in this session, e.g. no launchd GUI login session over ssh,
# or systemctl --user unreachable).
#
# No hostnames, user ids or IPs are recorded: logs name only loopback, ports
# and temp paths.
set -u

PROG=$(basename "$0")
BASHY_BIN="${BASHY_BIN:-bashy}"
O4_MODE="${O4_MODE:-user}"
O4_PORT="${O4_PORT:-22022}"
O4_ADMIN_PORT="${O4_ADMIN_PORT:-17779}"
O4_TIMEOUT_SECS="${O4_TIMEOUT_SECS:-120}"
O4_SSH_TIMEOUT="${O4_SSH_TIMEOUT:-15}"

fail()  { echo "FAIL: $*" >&2; }
info()  { echo ">> $*"; }
pass()  { echo ">> PASS $*"; }

usage_error() { fail "$*"; echo "usage: BASHY_BIN=/path/to/product/bashy $PROG" >&2; exit 2; }
skip()  { echo "SKIP: $*" >&2; exit 3; }

# --- 0. preflight -----------------------------------------------------------
case "$O4_MODE" in user|system) ;; *) usage_error "O4_MODE must be user or system";; esac
case "$O4_PORT,$O4_ADMIN_PORT" in *[^0-9,]*) usage_error "ports must be numeric";; esac

OS=$(uname -s)
case "$OS" in
  Darwin) OSNAME=darwin ;;
  Linux)  OSNAME=linux ;;
  *) usage_error "this script covers macOS/Linux; on Windows use scripts/sshd-managed-service-check.ps1" ;;
esac

for t in ssh sftp ssh-keygen; do
  command -v "$t" >/dev/null 2>&1 || usage_error "missing client tool: $t"
done
[ -x "$BASHY_BIN" ] || [ -n "$(command -v "$BASHY_BIN")" ] || usage_error "BASHY_BIN not found: $BASHY_BIN"

if [ -n "${O4_PRODUCT_DIR:-}" ]; then
  PRODUCT_DIR="$O4_PRODUCT_DIR"
else
  # dirname of the resolved binary (not `dirname $BASHY_BIN` when found via PATH).
  BASHY_ABS=$(command -v "$BASHY_BIN") || usage_error "cannot resolve BASHY_BIN: $BASHY_BIN"
  case "$BASHY_ABS" in /*) ;; *) BASHY_ABS="$PWD/$BASHY_ABS";; esac
  PRODUCT_DIR=$(dirname "$BASHY_ABS")
fi
for m in bashy bash sh outpost; do
  [ -f "$PRODUCT_DIR/$m" ] || usage_error "product dir $PRODUCT_DIR is missing member: $m"
done
BASHY_BIN="$PRODUCT_DIR/bashy"
OUTPOST_CANDIDATE="$PRODUCT_DIR/outpost"
info "product: $PRODUCT_DIR (bashy+bash+sh+outpost)"

# Refuse to run against live ports: the check must own its listener + admin.
tcp_open() { # host port -> 0 when something accepts
  (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null
}
if tcp_open 127.0.0.1 "$O4_PORT"; then fail "port $O4_PORT already in use — pick a free O4_PORT"; exit 2; fi
if tcp_open 127.0.0.1 "$O4_ADMIN_PORT"; then fail "admin port $O4_ADMIN_PORT already in use — pick a free O4_ADMIN_PORT"; exit 2; fi

ME=$(id -un)
UIDN=$(id -u)
# The service identity is one fixed name per platform
# (io.dhnt.outpost / outpost.service / outpost). In system mode the real
# `service install` path replaces same-name registrations (including the
# run-as user's own), so refuse rather than clobber a registration this
# check did not create. User-mode registrations live under the isolated
# $HOME/$XDG_CONFIG_HOME, so they shadow — never replace — real ones.
REALHOME=$(eval echo "~$ME")
guard_same_name_registration() {
  if [ "$O4_MODE" != system ]; then return 0; fi
  if [ "$OSNAME" = darwin ]; then
    if [ -e /Library/LaunchDaemons/io.dhnt.outpost.plist ] \
      || sudo -n launchctl print system/io.dhnt.outpost >/dev/null 2>&1; then
      fail "a real outpost LaunchDaemon is registered — remove it first (or run O4_MODE=user)"; exit 2
    fi
    if [ -e "$REALHOME/Library/LaunchAgents/io.dhnt.outpost.plist" ]; then
      fail "a real outpost LaunchAgent exists for $ME — system install would remove it; remove it first (or run O4_MODE=user)"; exit 2
    fi
  else
    if [ -e /etc/systemd/system/outpost.service ] \
      || sudo -n systemctl is-enabled outpost.service >/dev/null 2>&1; then
      fail "a real outpost systemd system unit is registered — remove it first (or run O4_MODE=user)"; exit 2
    fi
    if [ -e "$REALHOME/.config/systemd/user/outpost.service" ]; then
      fail "a real outpost systemd --user unit exists for $ME — system install would remove it; remove it first (or run O4_MODE=user)"; exit 2
    fi
  fi
}
guard_same_name_registration

# --- 1. isolated workspace --------------------------------------------------
T=$(mktemp -d "${TMPDIR:-/tmp}/o4-sshd-check-XXXXXX")
INSTALL="$T/install"
XDG_CONFIG_HOME="$T/config"; export XDG_CONFIG_HOME
XDG_CACHE_HOME="$T/cache";   export XDG_CACHE_HOME
BASHY_HOME="$T/bhome";       export BASHY_HOME
OUTPOST_ADMIN_ADDR="127.0.0.1:$O4_ADMIN_PORT"; export OUTPOST_ADMIN_ADDR
# HOME override keeps direct outpost/bashy state (config, plist staging,
# unit files) inside $T. The launchd/systemd-spawned daemon gets the real
# HOME from the manager — daemon config still resolves isolated because
# XDG_* (propagated below) win over $HOME/.config per conf/paths.go.
ISOLATED_HOME="$T/home"
mkdir -p "$ISOLATED_HOME" "$INSTALL"
export HOME="$ISOLATED_HOME"
# A running systemd --user manager fixes its unit search path at startup, so
# a unit written under the isolated XDG_CONFIG_HOME is invisible to
# `systemctl --user enable` ("Unit file ... does not exist"). In Linux user
# mode only the unit directory is shared with the real one; outpost config,
# cache and state stay isolated. Refuse rather than touch an existing unit.
REAL_UNIT_DIR_CREATED=0
if [ "$OSNAME" = linux ] && [ "$O4_MODE" = user ]; then
  if [ -e "$REALHOME/.config/systemd/user/outpost.service" ]; then
    fail "a real outpost systemd --user unit exists for $ME — remove it first"; exit 2
  fi
  [ -d "$REALHOME/.config/systemd/user" ] || REAL_UNIT_DIR_CREATED=1
  mkdir -p "$REALHOME/.config/systemd/user" "$XDG_CONFIG_HOME"
  ln -s "$REALHOME/.config/systemd" "$XDG_CONFIG_HOME/systemd"
fi

# The daemon's authorized_keys lookup uses the OS home directory (not $HOME),
# so resolve the real path via the account database — never $HOME here.
AK_REAL=$(eval echo "~$ME/.ssh/authorized_keys")
AK_BAK="$T/authorized_keys.bak"
AK_EXISTED=0
if [ -f "$AK_REAL" ]; then AK_EXISTED=1; cp -p "$AK_REAL" "$AK_BAK"; fi

LAUNCHD_SETENV=0; SYSTEMD_IMPORTED=0

cleanup() {
  rc=$?
  # Best-effort teardown in reverse order; never mask the check verdict
  # except when teardown itself leaves residue (then FAIL).
  if [ -n "${OUTPOST_INSTALLED:-}" ] && [ -x "$INSTALL/outpost" ]; then
    if [ "$OSNAME" = darwin ] && [ "$O4_MODE" = system ]; then
      sudo -n "$INSTALL/outpost" service uninstall --system >/dev/null 2>&1 || true
    elif [ "$O4_MODE" = user ] && [ "$OSNAME" != windows ]; then
      "$INSTALL/outpost" service uninstall --user >/dev/null 2>&1 || true
    else
      sudo -n "$INSTALL/outpost" service uninstall >/dev/null 2>&1 || true
    fi
  fi
  # Manager env propagation is transient: revert it.
  if [ "$LAUNCHD_SETENV" = 1 ]; then
    if [ "$O4_MODE" = system ]; then sudo -n launchctl unsetenv XDG_CONFIG_HOME XDG_CACHE_HOME OUTPOST_ADMIN_ADDR >/dev/null 2>&1 || true
    else launchctl unsetenv XDG_CONFIG_HOME XDG_CACHE_HOME OUTPOST_ADMIN_ADDR >/dev/null 2>&1 || true; fi
  fi
  if [ "$SYSTEMD_IMPORTED" = 1 ]; then
    systemctl --user unset-environment XDG_CONFIG_HOME XDG_CACHE_HOME OUTPOST_ADMIN_ADDR >/dev/null 2>&1 || true
    sudo -n systemctl unset-environment XDG_CONFIG_HOME XDG_CACHE_HOME OUTPOST_ADMIN_ADDR >/dev/null 2>&1 || true
  fi
  if [ "${REAL_UNIT_DIR_CREATED:-0}" = 1 ]; then
    rmdir "$REALHOME/.config/systemd/user" "$REALHOME/.config/systemd" 2>/dev/null || true
  fi
  # Belt and suspenders: SIGTERM anything still running from $INSTALL
  # (supervisord handles SIGTERM by stopping the daemon first), then SIGKILL.
  for sig in TERM KILL; do
    pids=$(ps -ax -o pid= -o command= 2>/dev/null | grep -F "$INSTALL/" | grep -v grep | awk '{print $1}') || true
    [ -n "$pids" ] || break
    # shellcheck disable=SC2086
    kill "-$sig" $pids 2>/dev/null || true
    sleep 2
  done
  # Restore the real authorized_keys byte-for-byte (or remove what we created).
  if [ "$AK_EXISTED" = 1 ]; then cp -p "$AK_BAK" "$AK_REAL"
  elif [ -f "$AK_REAL" ]; then rm -f "$AK_REAL"; rmdir "$(dirname "$AK_REAL")" 2>/dev/null || true; fi
  if [ "${O4_KEEPDIR:-0}" = 1 ]; then info "keeping $T (O4_KEEPDIR=1)"; else rm -rf "$T"; fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

# --- 2. ephemeral key + documented authorized_keys auth ---------------------
ssh-keygen -t ed25519 -f "$T/id" -N '' -C o4-check -q
mkdir -p "$(dirname "$AK_REAL")"; chmod 700 "$(dirname "$AK_REAL")"
cat "$T/id.pub" >>"$AK_REAL"; chmod 600 "$AK_REAL"
info "ephemeral key appended to the service user's authorized_keys (backed up, restored on exit)"

SSH_OPTS="-o BatchMode=yes -o IdentitiesOnly=yes -o PreferredAuthentications=publickey -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$T/known_hosts -o ConnectTimeout=$O4_SSH_TIMEOUT -o LogLevel=ERROR"

# --- 3. install pair + register managed service via bashy -------------------
propagate_manager_env() {
  # Make the manager-spawned daemon inherit our isolated XDG_*/admin env.
  # Where the manager refuses (no GUI login session for `bootstrap gui/`;
  # SIP forbids system-domain `launchctl setenv`), SKIP — the box cannot
  # host an isolated managed daemon in this session.
  if [ "$OSNAME" = darwin ]; then
    if [ "$O4_MODE" = system ]; then
      sudo -n true 2>/dev/null || skip "O4_MODE=system needs passwordless sudo"
      if ! sudo -n launchctl setenv XDG_CONFIG_HOME "$XDG_CONFIG_HOME" 2>"$T/setenv.err"; then
        skip "system-domain launchctl setenv refused ($(cat "$T/setenv.err" 2>/dev/null || echo denied) — under SIP an isolated system daemon is not possible in this session)"
      fi
      sudo -n launchctl setenv XDG_CACHE_HOME "$XDG_CACHE_HOME" >/dev/null 2>&1 || return 1
      sudo -n launchctl setenv OUTPOST_ADMIN_ADDR "$OUTPOST_ADMIN_ADDR" >/dev/null 2>&1 || return 1
    else
      launchctl print "gui/$UIDN" >/dev/null 2>&1 \
        || skip "no launchd GUI login session for this user in this session — run with O4_MODE=system (sudo) or from a login session"
      launchctl setenv XDG_CONFIG_HOME "$XDG_CONFIG_HOME" >/dev/null 2>&1 || return 1
      launchctl setenv XDG_CACHE_HOME "$XDG_CACHE_HOME" >/dev/null 2>&1 || return 1
      launchctl setenv OUTPOST_ADMIN_ADDR "$OUTPOST_ADMIN_ADDR" >/dev/null 2>&1 || return 1
    fi
    LAUNCHD_SETENV=1
  else
    # Linux.
    if [ "$O4_MODE" = user ]; then
      systemctl --user show-environment >/dev/null 2>&1 \
        || skip "systemd --user unreachable in this session (no user manager / no D-Bus)"
      systemctl --user import-environment XDG_CONFIG_HOME XDG_CACHE_HOME OUTPOST_ADMIN_ADDR >/dev/null || return 1
      SYSTEMD_IMPORTED=1
    else
      sudo -n true 2>/dev/null || skip "O4_MODE=system needs passwordless sudo"
      sudo -n systemctl set-environment "XDG_CONFIG_HOME=$XDG_CONFIG_HOME" "XDG_CACHE_HOME=$XDG_CACHE_HOME" "OUTPOST_ADMIN_ADDR=$OUTPOST_ADMIN_ADDR" >/dev/null || return 1
      SYSTEMD_IMPORTED=1
    fi
  fi
}

if [ "$O4_MODE" = user ]; then
  propagate_manager_env || { fail "manager env propagation failed"; exit 1; }
  # macOS default IS the per-user LaunchAgent; Linux needs the explicit
  # --user flag (default there is the system unit, which needs root).
  if [ "$OSNAME" = linux ]; then SVC_FLAGS="--service --user"; else SVC_FLAGS="--service"; fi
  # shellcheck disable=SC2086
  "$BASHY_BIN" self install --dir "$INSTALL" $SVC_FLAGS \
    || { fail "bashy self install --service failed"; exit 1; }
else
  propagate_manager_env || { fail "manager env propagation failed"; exit 1; }
  # bashy refuses --service as root, and a system registration needs admin:
  # the sanctioned split (see `bashy self install --service` root refusal)
  # is install-as-user, then register via the installed outpost with sudo.
  "$BASHY_BIN" self install --dir "$INSTALL" \
    || { fail "bashy self install failed"; exit 1; }
  sudo -n "$INSTALL/outpost" service install --system --run-as "$ME" \
    || { fail "outpost service install --system failed"; exit 1; }
fi
OUTPOST_BIN="$INSTALL/outpost"
OUTPOST_INSTALLED=1
pass "bashy installed the pair + registered the managed service (mode=$O4_MODE)"

# --- 4. point the managed daemon at the dedicated sshd port -----------------
# `config set` talks to the running daemon over its admin endpoint, so wait
# for the supervisor to bring the daemon up first. Listener binds are
# read once at boot: `config set` only saves (printing a restart advisory
# when the host is paired) and never applies live — on a first-run host
# it stays silent by design (admincore skips the advisory until pairing,
# so a setup batch doesn't re-exec N times). Restart explicitly so the
# new bind takes effect now; under the supervisor a restart is
# exit-and-respawn (OUTPOST_SUPERVISED=1).
wait_for_tcp() { # host port timeout_secs -> 0 on connect
  end=$((SECONDS + $3))
  while [ "$SECONDS" -lt "$end" ]; do
    if tcp_open "$1" "$2"; then return 0; fi
    sleep 2
  done
  return 1
}
wait_for_tcp 127.0.0.1 "$O4_ADMIN_PORT" "$O4_TIMEOUT_SECS" \
  || { fail "managed daemon admin never opened 127.0.0.1:$O4_ADMIN_PORT within ${O4_TIMEOUT_SECS}s (daemon log tail follows)"; tail -20 "$XDG_CACHE_HOME"/outpost/daemon.log 2>/dev/null; exit 1; }
pass "managed daemon admin answers on 127.0.0.1:$O4_ADMIN_PORT"
cfg_out=$("$OUTPOST_BIN" config set --ssh-listen-addr "127.0.0.1:$O4_PORT" 2>&1) \
  || { fail "config set --ssh-listen-addr failed: $cfg_out"; exit 1; }
info "config set: $cfg_out"
# Nothing auto-restarts for networking changes (the "Restarting" message is
# an advisory for the operator/SPA banner) — restart explicitly so the new
# bind takes effect now. Under the supervisor this is exit-and-respawn;
# `stop` is the fallback (the supervisor respawns the daemon either way).
"$OUTPOST_BIN" restart >/dev/null 2>&1 || "$OUTPOST_BIN" stop >/dev/null 2>&1 || true

# --- 5. managed sshd answers --------------------------------------------------
wait_for_tcp 127.0.0.1 "$O4_PORT" "$O4_TIMEOUT_SECS" \
  || { fail "managed sshd never opened 127.0.0.1:$O4_PORT within ${O4_TIMEOUT_SECS}s (daemon log tail follows)"; tail -20 "$XDG_CACHE_HOME"/outpost/daemon.log 2>/dev/null; exit 1; }
pass "managed sshd listens on 127.0.0.1:$O4_PORT"

if [ "$O4_MODE" = user ]; then "$OUTPOST_BIN" service status --user; else sudo -n "$OUTPOST_BIN" service status; fi

# --- 6. ssh in with the documented key auth, run a command ------------------
# -n: stdin is never the session (a `curl|bash` run must not feed the
# script body to ssh as its stdin).
# shellcheck disable=SC2086
out=$(ssh -n -p "$O4_PORT" -i "$T/id" $SSH_OPTS "$ME@127.0.0.1" 'echo o4-ok' 2>&1) \
  || { fail "ssh exec failed: $out"; exit 1; }
[ "$out" = "o4-ok" ] || { fail "ssh exec printed '$out', want 'o4-ok'"; exit 1; }
pass "ssh exec: echo o4-ok"

# --- 7. SFTP put/get round trip ----------------------------------------------
echo "o4-sftp-payload" >"$T/up.txt"
cat >"$T/sftp.batch" <<EOF
put $T/up.txt $T/rmt.txt
get $T/rmt.txt $T/down.txt
rm $T/rmt.txt
EOF
# shellcheck disable=SC2086
sftp -P "$O4_PORT" -i "$T/id" $SSH_OPTS -b "$T/sftp.batch" "$ME@127.0.0.1" </dev/null >/dev/null 2>&1 \
  || { fail "sftp round trip failed"; exit 1; }
cmp -s "$T/up.txt" "$T/down.txt" || { fail "sftp payload mismatch"; exit 1; }
[ -e "$T/rmt.txt" ] && { fail "sftp cleanup (rm) did not take effect"; exit 1; }
pass "sftp put/get round trip"

# --- 8. stop + uninstall; nothing may remain -----------------------------------
# `--system` exists only on darwin; the Linux system unit is the default.
if [ "$O4_MODE" = user ]; then UNINSTALL_FLAGS="--user"
elif [ "$OSNAME" = darwin ]; then UNINSTALL_FLAGS="--system"
else UNINSTALL_FLAGS=""; fi
if [ "$O4_MODE" = user ]; then
  # shellcheck disable=SC2086
  "$OUTPOST_BIN" service uninstall $UNINSTALL_FLAGS || { fail "service uninstall failed"; exit 1; }
else
  # shellcheck disable=SC2086
  sudo -n "$OUTPOST_BIN" service uninstall $UNINSTALL_FLAGS || { fail "service uninstall failed"; exit 1; }
fi
"$OUTPOST_BIN" stop >/dev/null 2>&1 || true
# SIGTERM the supervisor (it stops the daemon first), then verify.
SUP_PID_FILE="$XDG_CACHE_HOME/outpost/supervisord.pid"
if [ -f "$SUP_PID_FILE" ]; then
  kill -TERM "$(cat "$SUP_PID_FILE")" 2>/dev/null || true
fi
end=$((SECONDS + 60))
while [ "$SECONDS" -lt "$end" ]; do
  tcp_open 127.0.0.1 "$O4_PORT" || break
  sleep 2
done
if tcp_open 127.0.0.1 "$O4_PORT"; then fail "sshd port still open after uninstall"; exit 1; fi
leftovers=$(ps -ax -o pid= -o command= 2>/dev/null | grep -F "$INSTALL/" | grep -v grep || true)
[ -z "$leftovers" ] || { fail "processes still running from $INSTALL: $leftovers"; exit 1; }
# The registration itself must be gone, not just the processes.
if [ "$OSNAME" = linux ]; then
  if [ "$O4_MODE" = user ]; then
    systemctl --user is-enabled outpost.service >/dev/null 2>&1 && { fail "systemd --user unit still registered after uninstall"; exit 1; }
  else
    systemctl is-enabled outpost.service >/dev/null 2>&1 && { fail "systemd system unit still registered after uninstall"; exit 1; }
  fi
elif [ "$OSNAME" = darwin ] && [ "$O4_MODE" = system ]; then
  [ -e /Library/LaunchDaemons/io.dhnt.outpost.plist ] && { fail "LaunchDaemon still registered after uninstall"; exit 1; }
fi
OUTPOST_INSTALLED=""  # cleanup trap: nothing left to uninstall
pass "service uninstalled; no registration, no listener, no processes, authorized_keys restored"
echo "O4-OK mode=$O4_MODE os=$OSNAME port=$O4_PORT"
