#!/usr/bin/env bash
# wrapper.sh — bare-metal deploy with atomic swap + rollback, mirroring the
# outpost self-upgrade worker (StageFromURL → verify → keep .previous → atomic
# rename → restart → verify). This is the "Stage 3 Deploy" of the reference CI/CD
# pipeline (docs/cicd-strategy.md Part 2). One env per invocation.
#
#   deploy/wrapper.sh --env qa --url https://.../hello-tessaro-linux-amd64 --sha256 <hex>
#   deploy/wrapper.sh --env prod --rollback
#   deploy/wrapper.sh --env dev            # no --url: build from source (local loop)
set -euo pipefail

ENV=""
URL=""
SHA256=""
ROLLBACK=0
INSTALL_DIR="${HELLO_TESSARO_INSTALL_DIR:-$HOME/.local/bin}"
RESTART_CMD="${HELLO_TESSARO_RESTART_CMD:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --env) ENV="$2"; shift 2 ;;
    --url) URL="$2"; shift 2 ;;
    --sha256) SHA256="$2"; shift 2 ;;
    --rollback) ROLLBACK=1; shift ;;
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --restart-cmd) RESTART_CMD="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$ENV" ] || { echo "--env is required (dev|qa|prod)" >&2; exit 2; }

case "$ENV" in
  dev)  PORT=8080 ;;
  qa)   PORT=8081 ;;
  prod) PORT=8082 ;;
  *) echo "unknown env: $ENV" >&2; exit 2 ;;
esac

BIN="$INSTALL_DIR/hello-tessaro-$ENV"
PREV="$BIN.previous"
mkdir -p "$INSTALL_DIR"

restart() {
  if [ -n "$RESTART_CMD" ]; then
    eval "$RESTART_CMD"
  else
    # Default local restart: kill the exact PID on this env's port, relaunch.
    local pid; pid="$(lsof -ti "tcp:$PORT" 2>/dev/null || true)"
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    sleep 1
    APP_ENV="$ENV" HELLO_TESSARO_ADDR="127.0.0.1:$PORT" nohup "$BIN" >/tmp/hello-tessaro-$ENV.log 2>&1 &
  fi
}

verify() {
  # Poll /healthz then /version until the app is serving (10 min ceiling).
  local deadline=$(( $(date +%s) + 600 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
      curl -fsS "http://127.0.0.1:$PORT/version" 2>/dev/null && return 0
    fi
    sleep 2
  done
  return 1
}

if [ "$ROLLBACK" -eq 1 ]; then
  [ -f "$PREV" ] || { echo "no $PREV to roll back to" >&2; exit 1; }
  echo ">> rolling back $ENV: $PREV -> $BIN"
  mv -f "$PREV" "$BIN"           # atomic-ish swap back
  restart
  verify && { echo; echo ">> rollback OK"; exit 0; } || { echo ">> rollback FAILED health check" >&2; exit 1; }
fi

STAGED="$(mktemp)"
trap 'rm -f "$STAGED"' EXIT
if [ -n "$URL" ]; then
  echo ">> staging $URL"
  curl -fsSL "$URL" -o "$STAGED"
  if [ -n "$SHA256" ]; then
    echo "$SHA256  $STAGED" | shasum -a 256 -c - >/dev/null || { echo ">> sha256 mismatch — refusing" >&2; exit 1; }
  fi
else
  echo ">> no --url; building from source"
  ( cd "$(dirname "$0")/.." && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X main.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo local)" \
      -o "$STAGED" . )
fi
chmod +x "$STAGED"

# Probe the candidate reports a version before we swap it in.
"$STAGED" --version --json >/dev/null || { echo ">> candidate failed --version probe" >&2; exit 1; }

# Keep the current binary as .previous for one-command rollback.
[ -f "$BIN" ] && cp -f "$BIN" "$PREV"
mv -f "$STAGED" "$BIN"; trap - EXIT
echo ">> deployed $ENV; restarting"
restart
verify && { echo; echo ">> deploy OK ($ENV on :$PORT)"; } || {
  echo ">> health check FAILED — rolling back" >&2
  [ -f "$PREV" ] && { mv -f "$PREV" "$BIN"; restart; }
  exit 1
}
