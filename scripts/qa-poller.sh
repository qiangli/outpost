#!/usr/bin/env bash
# qa-poller — decentralized, tag-driven QA promotion (one poller per QA host).
#
# The git-tag graph IS the promotion state machine; there is no central
# orchestrator. This poller:
#   1. polls GitHub for the newest vX.Y.Z-dev tag,
#   2. if this host's OS hasn't promoted that version yet,
#   3. runs `bashy dag README.md qa` (downloads the built artifact + smokes it),
#   4. on PASS creates the promotion ref refs/qa/<ver>/<os> (a side namespace, so
#      `git tag -l` stays = releases only),
#   5. on FAIL reports via OTel so the dev conductor sees it and dispatches a fix.
# The prod poller is the same shape watching refs/qa/*; it tags the release
# vX.Y.Z once its CONFIGURED required-OS set is present (availability-driven).
#
# Host prereq: bashy only (self-provisions git/coreutils/go) + a GitHub token
# (GITHUB_TOKEN, else `bashy gh auth token`, else `bashy secrets env`).
# See dhnt docs project_sdlc_pipeline_4_crossplatform.
set -uo pipefail
export PATH="$HOME/bin:$PATH"
REPO="${OUTPOST_REPO:-qiangli/outpost}"
INTERVAL="${QA_POLL_INTERVAL:-300}"
ONCE="${QA_POLL_ONCE:-}"                      # set to run a single pass (for testing)
os=$(bashy uname -s | tr 'A-Z' 'a-z'); case "$os" in *darwin*) os=darwin;; *linux*) os=linux;; *) os=windows;; esac
export OTEL_SERVICE_NAME="${OTEL_SERVICE_NAME:-outpost-qa-$os}"

gh_ok(){ bashy gh auth token >/dev/null 2>&1 || [ -n "${GITHUB_TOKEN:-}" ] || eval "$(bashy secrets env 2>/dev/null)"; [ -n "${GITHUB_TOKEN:-}$(bashy gh auth token 2>/dev/null)" ]; }

pass(){                                        # one poll pass
  local dev ver ref sha
  # pick the highest vX.Y.Z-dev tag. awk only — bashy's pure-Go coreutils have no
  # `grep -o` and no `sort -V`, so this must not depend on either (works whether the
  # poller runs under system bash OR bashy = the real target userland).
  dev=$(bashy git ls-remote --tags "https://github.com/$REPO.git" 2>/dev/null | awk -F/ '
    /refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+-dev$/ {
      t=$NF; v=t; sub(/-dev$/,"",v); sub(/^v/,"",v); split(v,a,".")
      k=a[1]*1000000+a[2]*1000+a[3]; if (k>m){m=k; b=t}
    } END{ if (b) print b }')
  [ -n "$dev" ] || { echo "[$os] no -dev tag yet"; return 0; }
  ver="${dev%-dev}"; ref="refs/qa/${ver}/${os}"
  if bashy gh api "/repos/$REPO/git/$ref" >/dev/null 2>&1; then
    echo "[$os] $ver already promoted"; return 0
  fi
  echo ">> [$os] new $dev — running qa"
  if OUTPOST_TEST_VERSION="$dev" bashy dag README.md qa 2>&1 | tee "/tmp/qa-$os.log"; then
    sha=$(bashy git ls-remote "https://github.com/$REPO.git" "refs/tags/$dev" | awk '{print $1}' | head -1)
    if bashy gh api -X POST "/repos/$REPO/git/refs" -f "ref=$ref" -f "sha=$sha" >/dev/null 2>&1; then
      echo ">> [$os] PROMOTED $ref"
    else
      echo ">> [$os] WARN: could not create $ref (token/perms?)"
    fi
  else
    echo ">> [$os] QA FAILED $dev — see /tmp/qa-$os.log; report via OTel service=$OTEL_SERVICE_NAME"
    # TODO(otel): emit an OTLP log/span (version=$dev, os=$os, tail of the log) so the
    # dev conductor's query_logs/query_traces catches it and assigns the fleet to fix.
  fi
}

gh_ok || { echo "qa-poller: no GitHub token (set GITHUB_TOKEN / bashy gh auth login / bashy secrets)"; exit 2; }
if [ -n "$ONCE" ]; then pass; exit $?; fi
while true; do pass; sleep "$INTERVAL"; done
