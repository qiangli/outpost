# Building outpost from source

Every path below produces a `bin/outpost` (or `bin\outpost.exe`) with
the git commit stamped in, so `outpost version` reports a build you can
trace back to a SHA.

**The one thing to know first:** outpost's `go.mod` contains
`replace mvdan.cc/sh/v3 => ../sh` — the shell runner depends on a fork
that must exist as a *sibling directory* of the checkout. Every build
path below materializes it for you from the SHA pinned in
[`.sibling-pins`](../.sibling-pins). This is also why
`go install github.com/qiangli/outpost/cmd/outpost@latest` does **not**
work: Go refuses to `go install` a module with replace directives.

## Prerequisites

- **Go 1.25+** — <https://go.dev/dl/>, or:
  - macOS: `brew install go`
  - Windows: `winget install GoLang.Go`
  - Linux: your distro's package, or the tarball from go.dev
- **git** — *or* any installed outpost release (its embedded
  `outpost git` covers every repository operation; see
  ["Zero-git"](#zero-git-already-have-outpost-installed) below).

## macOS / Linux

```bash
git clone https://github.com/qiangli/outpost.git
cd outpost
./scripts/build.sh        # bootstraps ../sh, builds → ./bin/outpost
./bin/outpost version
```

## Windows (PowerShell)

```powershell
git clone https://github.com/qiangli/outpost.git
cd outpost
powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1   # → .\bin\outpost.exe
.\bin\outpost.exe version
```

The `-ExecutionPolicy Bypass` wrapper is needed because Windows'
default execution policy refuses to run `.ps1` files ("running scripts
is disabled on this system"); it bypasses the policy for this one
invocation without changing any system setting. Alternatives: relax the
policy once for your account
(`Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`) and call
`.\scripts\build.ps1` directly — or skip scripts entirely with
`outpost build` (below), which is a compiled binary and not subject to
execution policy. If you fetched the repo as a browser-downloaded ZIP
rather than `git clone`, also run
`Unblock-File .\scripts\build.ps1` to clear Mark-of-the-Web.

## Zero-git: already have outpost installed?

Any installed outpost (even an old release) can rebuild itself from
GitHub with one command — its embedded git client does the cloning, so
only the Go toolchain is required. Works identically on macOS, Linux,
and Windows:

```
outpost build                      # main → <user-cache>/outpost/build/outpost/bin/
outpost build --ref v0.3.0         # a tag
outpost build --ref 7fc8d10        # any commit SHA
outpost build --src .              # build the checkout you're standing in
outpost build -o ./outpost-new     # choose the output path
```

`outpost build` clones the repo, checks out `--ref`, materializes the
`../sh` sibling at its pinned SHA, and runs `go build` with provenance
ldflags. It prints the built path when done.

## Installing the result

To replace a live install, don't copy over the binary by hand — use the
upgrade flow, which probes the candidate and keeps a `.previous` for
rollback:

```
outpost upgrade --local ./bin/outpost     # or .\bin\outpost.exe
outpost version                           # confirm the new commit
outpost rollback                          # undo, if needed
```

On a machine with no outpost yet, just put the binary on PATH (e.g.
`~/bin` via `./scripts/install-bin.sh`, or
`%LOCALAPPDATA%\outpost\outpost.exe` where the Windows installer puts
it).

## Cross-compiling

```bash
./scripts/build-all.sh    # darwin/linux/windows × amd64/arm64 → ./bin/
PLATFORMS="windows-arm64" ./scripts/build-all.sh   # subset
```

Everything builds with `CGO_ENABLED=0` by default. The one exception
worth knowing: Linux **PAM** auth needs a cgo build
(`CGO_ENABLED=1` + `libpam-dev`) — see [`install.md`](install.md) for
that recipe and for the prebuilt-release installers.

## Building inside the dhnt umbrella

If your checkout is the `dhnt/outpost` submodule, `../sh` is already
mounted as the `dhnt/sh` submodule — every path above detects it and
leaves it alone. Nothing else changes.
