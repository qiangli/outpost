#!/bin/bash
# Supervises frpc in-place. Restarts frpc when the process exits or when its
# required local STCP visitor listeners remain absent after startup grace.

set -u

log() { printf '[runtime] %s\n' "$*" >&2; }

FRPC_BIN="${FRPC_BIN:-frpc}"
FRPC_CONFIG="${FRPC_CONFIG:-/tmp/frpc.toml}"
FRPC_LOG="${FRPC_LOG:-/tmp/frpc.log}"
FRPC_REQUIRED_PORTS="${FRPC_REQUIRED_PORTS:-}"
FRPC_STARTUP_GRACE_TICKS="${FRPC_STARTUP_GRACE_TICKS:-20}"
FRPC_MISS_THRESHOLD="${FRPC_MISS_THRESHOLD:-3}"
FRPC_PROBE_INTERVAL="${FRPC_PROBE_INTERVAL:-2}"
FRPC_RESTART_BACKOFF="${FRPC_RESTART_BACKOFF:-2}"
FRPC_RESTART_BACKOFF_MAX="${FRPC_RESTART_BACKOFF_MAX:-30}"
FRPC_PROBE_BIN="${FRPC_PROBE_BIN:-}"

FRPC_PID=""
ticks=0
misses=0
backoff="${FRPC_RESTART_BACKOFF}"
stopping=0

probe_port() {
    port="$1"
    if [ -n "${FRPC_PROBE_BIN}" ]; then
        "${FRPC_PROBE_BIN}" "${port}"
        return $?
    fi
    (echo >/dev/tcp/127.0.0.1/"${port}") 2>/dev/null
}

all_required_ports_up() {
    old_ifs="${IFS}"
    IFS=', '
    for port in ${FRPC_REQUIRED_PORTS}; do
        [ -z "${port}" ] && continue
        if ! probe_port "${port}"; then
            IFS="${old_ifs}"
            return 1
        fi
    done
    IFS="${old_ifs}"
    return 0
}

frpc_alive() {
    [ -n "${FRPC_PID}" ] && kill -0 "${FRPC_PID}" 2>/dev/null
}

stop_frpc() {
    if frpc_alive; then
        kill -TERM "${FRPC_PID}" 2>/dev/null || true
        wait "${FRPC_PID}" 2>/dev/null || true
    fi
    FRPC_PID=""
}

cleanup() {
    stopping=1
    stop_frpc
}

trap cleanup INT TERM EXIT

start_frpc() {
    log "starting frpc (matrix tunnel + STCP visitors)"
    "${FRPC_BIN}" -c "${FRPC_CONFIG}" >>"${FRPC_LOG}" 2>&1 &
    FRPC_PID=$!
    ticks=0
    misses=0
}

restart_frpc() {
    reason="$1"
    log "restarting frpc: ${reason}"
    tail -5 "${FRPC_LOG}" 2>/dev/null || true
    stop_frpc
    sleep "${backoff}"
    start_frpc
    if [ "${backoff}" -lt "${FRPC_RESTART_BACKOFF_MAX}" ]; then
        backoff=$((backoff * 2))
        if [ "${backoff}" -gt "${FRPC_RESTART_BACKOFF_MAX}" ]; then
            backoff="${FRPC_RESTART_BACKOFF_MAX}"
        fi
    fi
}

start_frpc
while [ "${stopping}" = "0" ]; do
    sleep "${FRPC_PROBE_INTERVAL}" || true
    [ "${stopping}" != "0" ] && break

    if ! frpc_alive; then
        restart_frpc "process exited"
        continue
    fi

    ticks=$((ticks + 1))
    if [ "${ticks}" -le "${FRPC_STARTUP_GRACE_TICKS}" ]; then
        continue
    fi

    if all_required_ports_up; then
        misses=0
        backoff="${FRPC_RESTART_BACKOFF}"
        continue
    fi

    misses=$((misses + 1))
    log "frpc required visitor listener missing (${misses}/${FRPC_MISS_THRESHOLD})"
    if [ "${misses}" -ge "${FRPC_MISS_THRESHOLD}" ]; then
        restart_frpc "required STCP listener missing"
    fi
done
