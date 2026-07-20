#!/usr/bin/env bash
# qa-poller-podman — Linux QA lane on a NON-Linux host via a local podman container.
#
# The third variant of the qa poller, for the case where you have NO dedicated
# remote Linux QA host but your (macOS/Windows) dev host DOES run podman. It
# validates the `linux` lane by running the per-OS smoke inside a throwaway
# alpine container on the local podman machine — the container produces the
# EVIDENCE (the built binary runs on a real Linux kernel), the broker (this
# host, which holds the token) authors refs/qa/<ver>/linux only on PASS.
#
# Same ATTESTED / BROKERED shape as qa-poller-broker.sh (docs/secretless-ci-
# shared-nodes.md, umbrella): the credential NEVER enters the container — the
# container downloads only the PUBLIC release asset; the broker performs the
# privileged write. The 'remote' here is just a short-lived local container.
#
#   trusted broker (this host, has token) ──podman run──► alpine container (no token)
#        │                                                     │ downloads the public
#        │                                                     │ asset + runs the smoke
#        └────── authors refs/qa/<ver>/linux ◄────────────────┘ reports REMOTE-QA-PASS
#
# Use it when you want the linux lane covered but can't spare a standing Linux
# box. The broker is its own host: it holds the token, so unlike the SSH broker
# there is no second machine to provision — only podman.
#
#   qa-poller.sh        — OWNED host, token local, smoke runs on the host itself.
#   qa-poller-broker.sh — token on broker, smoke on a REMOTE host over SSH.
#   qa-poller-podman.sh — token on broker, smoke in a LOCAL podman container (THIS).
#
# The container image is alpine (busybox wget does HTTPS; the outpost binary is
# built CGO_ENABLED=0, so the fully-static binary runs on musl unchanged — no
# glibc dep, no apk install needed). Override with QA_PODMAN_IMAGE if you need a
# different base (e.g. a hardened/sbom'd image); any image with wget/curl +
# sha256sum + awk will do.
#
# Config (env):
#   REPO            owner/repo of the release repo (default qiangli/outpost)
#   QA_PODMAN_IMAGE container image to run the smoke in (default alpine:3)
#   QA_ARCH         linux arch to validate (default: auto-detected from the podman
#                   machine — must match the machine's arch; see CAVEATS below)
#   QA_WORK         work dir for the smoke script + logs (default $HOME/.outpost-qa;
#                   MUST be under $HOME on macOS — the podman machine shares only $HOME)
#   PODMAN          podman binary (default `podman` on PATH)
#   QA_POLL_ONCE    set to run a single pass (for testing / scheduling)
#   QA_POLL_INTERVAL seconds between passes in loop mode (default 300)
#   QA_SMOKE_ONLY   set to run newest_dev + container_smoke ONCE and exit (operational
#                   debug: validate the lane WITHOUT touching refs; exit 0 on PASS)
set -uo pipefail
export PATH="$HOME/bin:$PATH"
REPO="${REPO:-qiangli/outpost}"
LANE=linux                                       # the container is always Linux
IMAGE="${QA_PODMAN_IMAGE:-alpine:3}"
PODMAN="${PODMAN:-podman}"
WORK="${QA_WORK:-$HOME/.outpost-qa}"
INTERVAL="${QA_POLL_INTERVAL:-300}"
ONCE="${QA_POLL_ONCE:-}"
SMOKE_ONLY="${QA_SMOKE_ONLY:-}"
export OTEL_SERVICE_NAME="${OTEL_SERVICE_NAME:-outpost-qa-podman-$LANE}"

gh_ok(){ bashy gh auth token >/dev/null 2>&1 || [ -n "${GITHUB_TOKEN:-}" ] || eval "$(bashy secrets env 2>/dev/null)"; [ -n "${GITHUB_TOKEN:-}$(bashy gh auth token 2>/dev/null)" ]; }

podman_ok(){ "$PODMAN" info >/dev/null 2>&1 || { echo "qa-poller-podman: podman not usable ('$PODMAN info' failed). On macOS start the machine: $PODMAN machine start"; return 1; }; }

# container_arch: the linux arch the podman machine actually presents. The
# container runs on the machine's real kernel, so the binary's arch MUST match
# it (cross-arch would need qemu/binfmt and is out of scope). Auto-detected from
# `podman info`; override with QA_ARCH only to pin to the machine's known arch.
container_arch(){
  local a="${QA_ARCH:-}"
  if [ -z "$a" ]; then
    a=$("$PODMAN" info -f '{{.Host.Arch}}' 2>/dev/null)
  fi
  case "$a" in
    x86_64|amd64)  echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) echo "$a" ;;
  esac
}

# newest_dev: highest vX.Y.Z-dev tag (awk only — the target userland has no
# `sort -V`/`grep -o`; mirrors qa-poller.sh / qa-poller-broker.sh).
newest_dev(){
  bashy git ls-remote --tags "https://github.com/$REPO.git" 2>/dev/null | awk -F/ '
    /refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+-dev$/ {
      t=$NF; v=t; sub(/-dev$/,"",v); sub(/^v/,"",v); split(v,a,".")
      k=a[1]*1000000+a[2]*1000+a[3]; if (k>m){m=k; b=t}
    } END{ if (b) print b }'
}

# write_smoke: emit the container-side smoke script once. It is bind-mounted
# read-only into the container (vars come via -e), which avoids quoting hell and
# keeps the container invocation a one-liner. Mirrors dag.md qa / the SSH broker's
# remote_smoke: download PUBLIC asset, sha-verify, run version+shell+git.
write_smoke(){
  mkdir -p "$WORK"
  cat > "$WORK/smoke.sh" <<'SMOKE'
#!/bin/sh
# container-side linux smoke (runs INSIDE the alpine container on the broker's
# podman machine). Downloads the PUBLIC release asset (NO credential — the
# broker holds the token; this container never sees it), sha-verifies it, and
# runs the same minimal smoke as dag.md qa / qa-poller-broker.sh remote_smoke.
# Prints REMOTE-QA-PASS on success. busybox wget does HTTPS on alpine; the
# outpost binary is static (CGO_ENABLED=0) so it runs on musl unchanged.
set -e
a="$ASSET"
# 1. download the PUBLIC asset + its sha256 sidecar (fail closed on either).
wget -q -O "$a"        "$BASE/$a"        || { echo "FAIL download $a"; exit 1; }
wget -q -O "$a.sha256" "$BASE/$a.sha256" || { echo "FAIL sha sidecar"; exit 1; }
# 2. verify sha256 — empty/mismatch is a hard failure (never run unverified bytes).
want=$(awk '{print $1}' "$a.sha256" | head -1)
got=$(sha256sum "$a" | awk '{print $1}' | head -1)
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "FAIL sha256 want=$want got=$got"; exit 1; }
chmod +x "$a"
# 3. executes + self-reports the expected version (== the upgrade worker's Probe;
#    a binary failing this is exactly what bricks a host on swap+re-exec).
vout=$(./"$a" version | head -1); echo "  version: $vout"
case "$vout" in *"$BASEV_NUM"*) ;; *) echo "FAIL version stamp ($vout)"; exit 1;; esac
# 4. the in-process shell engine runs (the /shell + /ssh surface a live host serves).
[ "$("./$a" shell -c 'echo runtime-ok')" = runtime-ok ] || { echo "FAIL shell"; exit 1; }
# 5. real-git surface resolves (Windows-without-system-git relies on it).
./"$a" git --version >/dev/null 2>&1 || { echo "FAIL git"; exit 1; }
rm -f "$a" "$a.sha256"
echo REMOTE-QA-PASS
SMOKE
}

# container_smoke <dev-tag> <basev>: run the linux smoke in a throwaway alpine
# container. The container downloads the public asset itself (no credential in
# the container) and reports REMOTE-QA-PASS on success. stdout is the evidence.
container_smoke(){
  local dev="$1" basev="$2"
  local asset="outpost-${basev}-${LANE}-${RARCH}"
  local base="https://github.com/${REPO}/releases/download/${dev}"
  "$PODMAN" run --rm \
    -e ASSET="$asset" -e BASE="$base" -e BASEV_NUM="${basev#v}" \
    -v "$WORK/smoke.sh:/qa/smoke.sh:ro" --workdir /tmp \
    "$IMAGE" sh /qa/smoke.sh
}

pass(){
  local dev ver ref sha
  dev=$(newest_dev)
  [ -n "$dev" ] || { echo "[$LANE] no -dev tag yet"; return 0; }
  ver="${dev%-dev}"; ref="refs/qa/${ver}/${LANE}"
  if bashy gh api "/repos/$REPO/git/$ref" >/dev/null 2>&1; then
    echo "[$LANE] $ver already promoted"; return 0
  fi
  echo ">> [$LANE] new $dev — validating in podman ($IMAGE, $LANE/$RARCH)"
  if container_smoke "$dev" "$ver" 2>&1 | tee "$WORK/qa-podman-$LANE.log" | grep -q '^REMOTE-QA-PASS'; then
    sha=$(bashy git ls-remote "https://github.com/$REPO.git" "refs/tags/$dev" | awk '{print $1}' | head -1)
    if bashy gh api -X POST "/repos/$REPO/git/refs" -f "ref=$ref" -f "sha=$sha" >/dev/null 2>&1; then
      echo ">> [$LANE] PROMOTED $ref (attested by podman/$IMAGE)"
    else
      echo ">> [$LANE] WARN: QA passed but could not author $ref (token/perms?)"
    fi
  else
    echo ">> [$LANE] QA FAILED $dev — see $WORK/qa-podman-$LANE.log; report via OTel service=$OTEL_SERVICE_NAME"
  fi
}

gh_ok || { echo "qa-poller-podman: no GitHub token on this broker (bashy gh auth login)"; exit 2; }
podman_ok || exit 2
RARCH=$(container_arch)
[ -n "$RARCH" ] || { echo "qa-poller-podman: could not detect podman machine arch (set QA_ARCH)"; exit 2; }
write_smoke
echo "[$LANE] podman lane: machine=$("$PODMAN" info -f '{{.Host.OS}}/{{.Host.Arch}}' 2>/dev/null) → $LANE/$RARCH, image=$IMAGE, work=$WORK"

# QA_SMOKE_ONLY: validate the lane end-to-end WITHOUT authoring any ref — runs
# newest_dev + container_smoke once and exits 0 on PASS. Use it to confirm a new
# podman install / image / arch can do the lane before scheduling the poller.
if [ -n "$SMOKE_ONLY" ]; then
  dev=$(newest_dev); [ -n "$dev" ] || { echo "[$LANE] no -dev tag yet"; exit 0; }
  ver="${dev%-dev}"
  echo ">> [$LANE] SMOKE_ONLY $dev ($LANE/$RARCH)"
  container_smoke "$dev" "$ver"; exit $?
fi

if [ -n "$ONCE" ]; then pass; exit $?; fi
while true; do pass; sleep "$INTERVAL"; done
