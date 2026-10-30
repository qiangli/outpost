# S515.1d: accepted upgrade never swaps

2026-10-30 · Sprint #515 · Story #2761 (`364e4f74a14d`). Local investigation
only; no host changes, release, or live retry.

## Finding

The recorded request uses the **bashy repository commit** `51271e35` as
`Envelope.Commit`. The candidate is outpost, whose commit is
`4da588d684ac143f5ca2b9bdd9f231b83daff5b3`. `Probe` rejects this mismatch
before `SwapAtomic`/`ApplyPair` and before the restart callback. SHA-256
verification and build-identity verification are separate checks; a correct
checksum does not override the wrong commit.

The request is recorded in cloudbox commit
`3ff6167173bf6d508eed11343c2dda10332527d6`,
`docs/s515-1-live-round6.md`, P2 section. Only non-host fields are reproduced:

```json
{
  "release_id": "v1.0.0-rc.23-dev",
  "url": "https://github.com/qiangli/bashy/releases/download/v1.0.0-rc.23-dev/outpost-v1.0.0-rc.23-linux-amd64",
  "sha256": "429029f13cc4e4c7f939a48e44794d1a2ceb3364be6206bec1c62a847fc5c3f4",
  "commit": "51271e35"
}
```

The correction is `commit: "4da588d684ac143f5ca2b9bdd9f231b83daff5b3"`.
Do not remove the probe check or substitute a release tag for the commit.
The failed attempt does not poison the release-ID replay guard: a corrected
request can reuse the release ID. No corrected request was sent here.

## Evidence and limits

- Downloaded the published Linux amd64 asset locally, without executing it.
  `shasum -a 256` matched the request exactly. Static inspection found the
  full outpost commit above in the binary.
- The [rc.23-dev go.mod](https://github.com/qiangli/bashy/blob/v1.0.0-rc.23-dev/go.mod)
  pins outpost at `4da588d684ac`. Its
  [release configuration](https://github.com/qiangli/bashy/blob/v1.0.0-rc.23-dev/.goreleaser.yaml)
  stamps outpost's `ldCommit` from `OUTPOST_COMMIT`, resolved from that pin,
  independently of the bashy commit.
- The rc.10-dev go.mod pins outpost at `5456b706233e`. Inspection of
  `worker.go` and `probe.go` at that revision confirms the same commit guard,
  failure ledger, replay-guard reset, and explicit restart after swap.
- The worker returns `202 / staging candidate` immediately after starting
  a goroutine. It proves acceptance, not download completion. A bashy
  release URL also downloads/checks/extracts the full product archive before
  probing. Neither a completed stage nor its last failure was observed on
  the live host in the original evidence.
- The wrong commit is a deterministic pre-swap blocker and explains the
  reported behavior. Without the host ledger, an additional earlier network
  or extraction failure cannot be excluded. This is not a claim to have
  observed the host's `probe_failed` entry.
- Semver and health confirmation do not explain this request: rc.10's
  worker already accepted it; health confirmation is armed after swap.

## Agent-readable result already available

Use MCP `outpost_upgrade_history` with `{"limit": 5}` (newest entries last),
the `outpost://upgrade-history` MCP resource, or:

```sh
outpost upgrade history --json --limit 5
```

These exist in rc.10-dev and return `release_id`, `step`, `error`, and `at`.
For this mismatch, expect `probe_failed` with expected `51271e3` and actual
`4da588d`. `swap_done` means the swap landed and restart was scheduled; verify
the running version/start time separately. A lone `received` entry is not
proof of a completed stage. No new status API is needed for this incident.

## Change and validation

The mismatch diagnostic now explicitly says to supply the candidate's
outpost commit from `version --json`, rather than the product repository
commit. The envelope field comment documents short/full SHA support.

`TestWorker_ProductCommitMismatchThenCorrectedRetry` serves a native test
executable over local TLS with a valid checksum. It checks acceptance followed
by `probe_failed`, an unchanged live binary and zero restarts; then changes
only the envelope commit and checks `swap_done`, installed candidate bytes,
and one restart. It uses temporary files and no service or public network.
It failed before the diagnostic change and passed afterward. The existing
swap logic needed no change; the failed assertion was the missing actionable
diagnostic, not a missing swap trigger.

Final gates on the local Darwin arm64 workspace:

- `make test` (`go test -tags fb_archives -short ./...`): PASS; every package
  reported `ok` or `[no test files]`, including CLI, MCP, admin UI and upgrade.
- `go test -race -tags fb_archives ./internal/agent/upgrade`: PASS.
- `./scripts/fmtcheck.sh`: PASS (848 files); `git diff --check`: PASS.

The end-to-end worker regression uses a native test executable. It does not
claim a Linux service restart or paired-release download was exercised locally.
