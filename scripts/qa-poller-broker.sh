#!/usr/bin/env bash
# qa-poller-broker — the ATTESTED / BROKERED variant of qa-poller.sh.
#
# Runs on a TRUSTED host that holds the GitHub credential. It validates a target
# OS lane on a possibly-UNTRUSTED / shared REMOTE host over SSH (which holds NO
# credential), and — only on PASS — authors the promotion ref itself. This is the
# "attested result, brokered write" pattern: the remote produces the evidence
# (the built binary runs on the real OS); the broker verifies it and performs the
# privileged write. See docs/secretless-ci-shared-nodes.md (umbrella).
#
#   trusted broker (this host, has token) ──ssh──► remote QA host (no token)
#        │                                              │ runs the per-OS smoke
#        └────── authors refs/qa/<ver>/<os> ◄───────────┘ reports PASS/FAIL
#
# Use it for a shared host that must not hold a write credential (e.g. a shared
# Windows box) — the standing token-holding qa-poller.sh is for OWNED hosts.
#
# Config (env):
#   REPO           owner/repo of the release repo (default qiangli/outpost)
#   QA_LANE        the OS lane this broker validates: linux|darwin|windows (required)
#   QA_REMOTE      ssh host alias of the remote QA machine (required)
#   QA_REMOTE_ARCH amd64|arm64 of the remote (default amd64)
#   QA_REMOTE_BASHY path to a CURRENT bashy on the remote (default: `bashy` on PATH)
#   QA_POLL_ONCE   set to run a single pass (for testing / scheduling)
#   QA_POLL_INTERVAL seconds between passes in loop mode (default 300)
set -uo pipefail
export PATH="$HOME/bin:$PATH"
REPO="${REPO:-qiangli/outpost}"
LANE="${QA_LANE:?set QA_LANE to the OS lane to validate (linux|darwin|windows)}"
REMOTE="${QA_REMOTE:?set QA_REMOTE to the ssh host of the remote QA machine}"
RARCH="${QA_REMOTE_ARCH:-amd64}"
RBASHY="${QA_REMOTE_BASHY:-bashy}"
INTERVAL="${QA_POLL_INTERVAL:-300}"
ONCE="${QA_POLL_ONCE:-}"
export OTEL_SERVICE_NAME="${OTEL_SERVICE_NAME:-outpost-qa-broker-$LANE}"

gh_ok(){ bashy gh auth token >/dev/null 2>&1 || [ -n "${GITHUB_TOKEN:-}" ] || eval "$(bashy secrets env 2>/dev/null)"; [ -n "${GITHUB_TOKEN:-}$(bashy gh auth token 2>/dev/null)" ]; }

# newest_dev: highest vX.Y.Z-dev tag (awk only — the target userland has no
# `sort -V`/`grep -o`; mirrors qa-poller.sh).
newest_dev(){
  bashy git ls-remote --tags "https://github.com/$REPO.git" 2>/dev/null | awk -F/ '
    /refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+-dev$/ {
      t=$NF; v=t; sub(/-dev$/,"",v); sub(/^v/,"",v); split(v,a,".")
      k=a[1]*1000000+a[2]*1000+a[3]; if (k>m){m=k; b=t}
    } END{ if (b) print b }'
}

# remote_smoke <basev>: run the per-OS release smoke on the remote host over SSH.
# The remote downloads the PUBLIC release asset (no credential) via its own CURRENT
# bashy (bashy curl works cross-platform since v0.18.0), sha-verifies it, and runs
# the same three checks the upgrade worker's Probe runs. Prints REMOTE-QA-PASS on
# success. Per-tool smokes plug in here (outpost: version+shell+git); a bashy/ycode
# broker overrides this function with its own tool's checks.
remote_smoke(){
  local basev="$1" ext=""
  [ "$LANE" = windows ] && ext=.exe
  local asset="outpost-${basev}-${LANE}-${RARCH}${ext}"
  local base="https://github.com/${REPO}/releases/download/${basev}-dev"
  ssh -o BatchMode=yes -o ConnectTimeout=15 "$REMOTE" "
    B='$RBASHY'
    a='$asset'; base='$base'
    \$B curl -fsSL -o \$a \$base/\$a || { echo 'FAIL download'; exit 1; }
    \$B curl -fsSL -o \$a.sha256 \$base/outpost-${basev}-${LANE}-${RARCH}.sha256 || { echo 'FAIL sha download'; exit 1; }
    want=\$(awk '{print \$1}' \$a.sha256 | head -1)
    got=\$(\$B sha256sum \$a | awk '{print \$1}' | head -1)
    [ -n \"\$want\" ] && [ \"\$want\" = \"\$got\" ] || { echo \"FAIL sha256 want=\$want got=\$got\"; exit 1; }
    v=\$(./\$a version | head -1); echo \"  version: \$v\"
    case \"\$v\" in *${basev#v}*) ;; *) echo 'FAIL version stamp'; exit 1;; esac
    [ \"\$(./\$a shell -c 'echo runtime-ok')\" = runtime-ok ] || { echo 'FAIL shell'; exit 1; }
    ./\$a git --version >/dev/null 2>&1 || { echo 'FAIL git'; exit 1; }
    rm -f \$a \$a.sha256
    echo REMOTE-QA-PASS
  "
}

pass(){
  local dev ver ref sha
  dev=$(newest_dev)
  [ -n "$dev" ] || { echo "[$LANE] no -dev tag yet"; return 0; }
  ver="${dev%-dev}"; ref="refs/qa/${ver}/${LANE}"
  if bashy gh api "/repos/$REPO/git/$ref" >/dev/null 2>&1; then
    echo "[$LANE] $ver already promoted"; return 0
  fi
  echo ">> [$LANE] new $dev — validating on $REMOTE ($LANE/$RARCH)"
  if remote_smoke "$ver" 2>&1 | tee "/tmp/qa-broker-$LANE.log" | grep -q '^REMOTE-QA-PASS'; then
    sha=$(bashy git ls-remote "https://github.com/$REPO.git" "refs/tags/$dev" | awk '{print $1}' | head -1)
    if bashy gh api -X POST "/repos/$REPO/git/refs" -f "ref=$ref" -f "sha=$sha" >/dev/null 2>&1; then
      echo ">> [$LANE] PROMOTED $ref (attested by $REMOTE)"
    else
      echo ">> [$LANE] WARN: QA passed but could not author $ref (token/perms?)"
    fi
  else
    echo ">> [$LANE] QA FAILED $dev on $REMOTE — see /tmp/qa-broker-$LANE.log; report via OTel service=$OTEL_SERVICE_NAME"
  fi
}

gh_ok || { echo "qa-poller-broker: no GitHub token on this broker (bashy gh auth login)"; exit 2; }
if [ -n "$ONCE" ]; then pass; exit $?; fi
while true; do pass; sleep "$INTERVAL"; done
