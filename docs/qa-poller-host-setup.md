# QA poller host setup (release verification)

A **QA host** verifies each `vX.Y.Z-dev` *pre-release* for its own OS/arch and,
on pass, creates the promotion ref `refs/qa/<version>/<os>`. `promote.yml` gates
the bare-tag promotion on those refs, so a release ships only after every
required-OS QA host has proven the exact bytes run. The poller is
[`scripts/qa-poller.sh`](../scripts/qa-poller.sh) — the git-tag graph *is* the
state machine; there is no central orchestrator.

```
 dev host (builds vX.Y.Z-dev)  ──►  QA host(s) (this doc)  ──►  bare tag vX.Y.Z (promote → rollout)
```

Run one poller per QA host. A single Windows QA host is enough if the dev host
covers the other platforms by being that platform (e.g. a macOS dev host is
continuously dev-tested, so the explicit QA gate can be Windows-only — see
`promote.yml`'s `required_os`).

### Variants — one per trust/hosting shape

- **[`qa-poller.sh`](../scripts/qa-poller.sh)** (this doc) — *owned* host: the
  token and the smoke both run on this host.
- **[`qa-poller-broker.sh`](../scripts/qa-poller-broker.sh)** — *attested /
  brokered*: the token stays on a trusted broker, the smoke runs on a **remote**
  host over SSH (for a shared/untrusted node that must not hold a write
  credential — e.g. a shared Windows box; see `docs/secretless-ci-shared-nodes.md`).
- **[`qa-poller-podman.sh`](../scripts/qa-poller-podman.sh)** — *attested /
  brokered*: the token stays on the broker, the smoke runs in a **local podman**
  container. Covers the `linux` lane when you have podman but no dedicated Linux
  host (e.g. a macOS dev host validating the Linux build) — see
  [Linux lane via podman](#linux-lane-via-podman-no-dedicated-linux-host).

## Prerequisites

- **bashy** on the host (it self-provisions git / coreutils / gh / curl).
- A **GitHub credential that can create refs** — a fine-grained PAT with
  repository **Contents: Read and write** on the release repo (classic `repo`
  scope also works).

## 1. GitHub auth — pick one

The poller's readiness check (`gh_ok`) tries, in order: `bashy gh auth token`,
then `$GITHUB_TOKEN`, then `bashy secrets env`.

### Recommended: `gh` keyring

Simplest and cross-platform — the token lives in the OS keyring, not a file:

```bash
# interactive (device/browser flow):
bashy gh auth login
# or non-interactive with a PAT:
bashy gh auth login --with-token < /path/to/token
bashy gh auth status          # verify: "Logged in ... (keyring)"
bashy gh api /repos/<owner>/<repo> --jq .full_name   # verify repo access
```

The poller then uses `bashy gh api` for every call, including creating the
promotion ref. Nothing else is needed.

### Alternative: cloudbox secrets vault (`bashy secrets`)

Reference a vaulted token by name instead of storing it on the host:

```
# ~/.config/bashy/secrets.map  (no secret — just an @reference)
GITHUB_TOKEN=@<vault-name>
```

plus a `secrets:read` token at `~/.config/bashy/secrets-token`. **On Windows
this path is currently unreliable inside the poller** — see Gotchas — so prefer
the `gh` keyring on Windows.

## 2. Place the poller

```bash
mkdir -p ~/.outpost-qa
cp scripts/qa-poller.sh ~/.outpost-qa/qa-poller.sh   # or fetch it from the repo
```

The poller writes a cwd-local `.qa/` log dir, so it must run **from a writable
work dir** (not a read-only or system dir).

## 3. Verify one pass

```bash
cd ~/.outpost-qa
QA_POLL_ONCE=1 bashy qa-poller.sh
# → "[<os>] <ver> already promoted"   (nothing to do), or
# → ">> [<os>] PROMOTED refs/qa/<ver>/<os>"   (QA passed, ref created)
```

## 4. Schedule it (standing poller)

Use the bashy scheduler (cross-platform; no OS-specific cron/Task Scheduler):

```bash
# a tiny wrapper keeps the cwd + one-shot flag in one place:
cat > ~/.outpost-qa/run-qa.sh <<'EOF'
cd "<work-dir>" || exit 1
QA_POLL_ONCE=1 bashy qa-poller.sh
EOF

bashy schedule add --every 15m --name outpost-qa-<os> -- <bashy-abs-path> <run-qa.sh-abs-path>
bashy schedule start          # background service
bashy schedule status         # running (pid=…)
bashy schedule list           # shows next fire time
```

`schedule start` runs a background service; it does **not** auto-start on
reboot. For a truly standing poller, re-run it after reboot, or supervise it
(e.g. an OS startup entry, or fold it into the outpost daemon's service
supervision).

## Windows gotchas

Windows QA hosts hit a few path quirks — all worked around above:

- **`outpost scp host:file` lands in the daemon's cwd** (typically
  `C:\Windows\System32`), *not* the OS-user home. After copying, move files
  explicitly to `%USERPROFILE%` (use forward-slash native paths for coreutils
  ops, e.g. `bashy mv "C:/Windows/System32/foo" "C:/Users/<you>/.config/..."`).
- **`$HOME` is msys-form inside a nested `bashy <script>`** (`/c/Users/<you>`),
  and `bashy secrets` mis-resolves it to `\c\Users\<you>` — so it can't find
  `secrets.map` and degrades to "no binding template". `gh` is unaffected (it
  reads its config from the keyring / `%AppData%` via the native user profile),
  which is the main reason to prefer the `gh` keyring path on Windows.
- **Prefer forward-slash native paths** (`C:/Users/<you>/…`) for `bashy`
  coreutils file operations; the msys `/c/...` form is not always converted.

## Linux lane via podman (no dedicated Linux host)

If you don't have a standing Linux QA machine but your dev host runs **podman**
(macOS or Windows), [`scripts/qa-poller-podman.sh`](../scripts/qa-poller-podman.sh)
covers the `linux` lane: it runs the per-OS smoke inside a throwaway **alpine**
container on the local podman machine and authors `refs/qa/<version>/linux` on
pass. Same attested/brokered shape as the SSH broker — the GitHub credential
stays on the broker (this host); the container downloads only the *public*
release asset.

```
 broker (this host, has token) ──podman run──► alpine container (no token)
      │                                             │ downloads public asset + smokes
      └──── authors refs/qa/<ver>/linux ◄───────────┘ reports REMOTE-QA-PASS
```

The container image is `alpine:3` (busybox `wget` does HTTPS; the outpost binary
is built `CGO_ENABLED=0`, so the fully-static binary runs on musl unchanged — no
`apk install` needed). Override the base with `QA_PODMAN_IMAGE` if you need a
hardened/SBOM'd image (any image with a working HTTPS downloader + `sha256sum` +
`awk` will do).

### Prerequisites

- **bashy** + **podman** on the host, and the podman machine running
  (`podman machine start` on macOS/Windows; on Linux podman runs rootless, no
  machine).
- A **GitHub credential that can create refs** on the release repo (same as the
  standing poller — `bashy gh auth login`).

### Verify the lane (no ref authoring)

`QA_SMOKE_ONLY=1` runs `newest_dev` + the container smoke **once** and exits,
*without* authoring any ref — use it to confirm a fresh podman install / image /
arch can do the lane before scheduling:

```bash
QA_SMOKE_ONLY=1 bash scripts/qa-poller-podman.sh
# → ">> [linux] SMOKE_ONLY vX.Y.Z-dev (linux/<arch>)" … "REMOTE-QA-PASS"
```

### Run it

```bash
QA_POLL_ONCE=1 bash scripts/qa-poller-podman.sh   # one pass (authors the ref on PASS)
# or schedule it like the standing poller (QA_POLL_ONCE=1 under bashy schedule)
```

### Caveats

- **The lane is always `linux`.** The container is a Linux kernel, so the binary
  must be the `linux/<arch>` build whose arch **matches the podman machine** —
  it is auto-detected (`podman info`). Validating a *different* linux arch needs
  a matching podman machine (cross-arch would need qemu/binfmt and is out of
  scope); pin with `QA_ARCH` only to the machine's own arch.
- **`QA_WORK` must be under `$HOME` on macOS** — the podman machine shares only
  the user's home dir, so the smoke script (bind-mounted into the container)
  can't live under `/tmp`. The default `$HOME/.outpost-qa` is correct; only
  override with another `$HOME`-relative path.

## How promotion consumes the refs

`promote.yml` reads `refs/qa/<version>/<os>` for each OS in its `required_os`
set and refuses to promote until they're all present. See
[`docs/cicd-strategy.md`](../../docs/cicd-strategy.md) (umbrella) for the full
two-tag release flow (`-dev` build+QA → bare-tag byte-promote → rollout).
