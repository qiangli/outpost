---
name: unblock-outpost-promotion
description: >
  Get a stuck outpost release promoted when a required-OS QA lane is not
  attesting — WHEN `bashy dag dag.md promote` reports BLOCKED, a QA host is
  offline or replaced, or `refs/qa/<version>/<os>` stops appearing. Decides which
  host substitutes for a dead one and repairs lane config; never invents a new
  release path. Keywords: promote, promotion blocked, QA lane, attestation,
  refs/qa, poller, QA_REMOTE, offline QA host, release stuck, byte-promote.
metadata:
  check-gate: bashy dag dag.md promote
  check-lanes: git ls-remote origin 'refs/qa/*'
  step-peers: outpost peers status
  step-wrapper: cat "$HOME/.outpost-qa/run-qa-windows.sh"
  step-runlane: bashy "$HOME/.outpost-qa/run-qa-windows.sh"
  step-verify: bashy dag dag.md promote
---

# Unblock outpost promotion

`bashy dag dag.md promote` refuses to promote because a required-OS lane has no
`refs/qa/<version>/<os>`. That refusal is CORRECT — your job is to make the lane
attest honestly, not to get past the gate.

## Contract

- `gate-green` — every OS in `REQUIRED_OS` has an attestation for the version,
  and `bashy dag dag.md promote` exits 0.
- `attested-by-that-os` — each attestation was produced by a host ACTUALLY
  running the OS it names.

## Two rules that outrank convenience

**Never loosen `REQUIRED_OS`.** It removes the gate instead of satisfying it,
and looks like success. A dead gate at least fails; an absent one ships.

**Never add a parallel mechanism.** The flow is `release.yml` → standing pollers
→ `promote.yml`, joined by `dag promote`. When it breaks, the cause is almost
always config drift on a host, not a missing component. A second promotion path
is how you get two mechanisms and no owner.

## Diagnose in this order — cheapest first

1. **Is the lane's QA host alive?** `outpost peers status`. An offline host is
   the single most likely cause, and it is invisible from the repo: promotion
   silently freezes at whatever version that host last attested. If the last
   promoted tag equals the offline host's version, you have your answer.

2. **Is there another host of the same OS/arch?** Then substitute it — edit
   `QA_REMOTE` in `~/.outpost-qa/run-qa-<lane>.sh`. That is the whole fix; do not
   redesign anything around it. Prefer an `owned` host over a `shared` one.

3. **Does the substitute have bashy?** The smoke needs it for `curl` and
   `sha256sum`. **On Windows, `where bashy` and `bashy --version` LIE over
   `outpost ssh`** — the session's PATH lacks the install dir, so both report
   nothing while bashy is installed. Search the filesystem instead:
   `outpost ssh <host> '"$COMSPEC" /c "dir /b /s C:\Users\*bashy.exe"'`
   Then set `QA_REMOTE_BASHY` to an absolute path with FORWARD slashes —
   backslashes are escapes in the remote bash and will be mangled.

4. **Is the wrapper still self-updating?** Compare `~/.outpost-qa/run-qa*.sh`
   against the documented form in `docs/qa-poller-host-setup.md`. It must
   re-fetch the canonical poller from the repo on every run. If that line is
   missing, the host has been executing a frozen snapshot and has silently missed
   every fix since the day it was edited. Restore the line; do not patch the
   local copy.

## Traps that cost a day

- **A lane can attest under the WRONG OS name.** Locale-broken `uname`
  normalization made a Mac call itself the `windows` lane. Had that run passed it
  would have authored a Windows attestation from macOS, and `promote.yml` would
  have trusted it. Check the log's `service=outpost-qa-<os>` matches the host.
- **`./binary.exe` does not execute in outpost's Windows bash** — it reports
  `not found`. Run it by absolute path. A failure here surfaces as an empty
  version string and `FAIL version stamp`, which reads like a bad build.
- **A green build is not an attestation.** `release.yml`'s smoke runs on a CI
  runner; the attestation runs the PUBLISHED artifact on a real host. Do not
  substitute one for the other.
- **Cross-compiling is not runtime testing.** That is the entire reason this gate exists.

## Then

Run the lane (`step-runlane`), confirm `REMOTE-QA-PASS` and that it authored
`refs/qa/<version>/<os>`, then `step-verify`. If you had to substitute a host,
say so in the promotion report — the next person needs to know which machine
vouched for these bytes.
