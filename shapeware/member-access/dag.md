---
name: member-access
description: Resolve, build, test and operate the member-access reference.
type: dag
default: test
---
# Member access lifecycle

Run from this folder on a disposable gate host. Only source may access the network; test services must be loopback fakes. Go 1.27, Git, bashy and Python 3 are required.

## Tasks

### source
Resolve the containing implementation or clone the pinned reference when standalone.
Effects: read, write, net
Band: none

```bash
set -eu
mkdir -p .shapeware
top=$(git rev-parse --show-toplevel 2>/dev/null || true)
if [ -n "$top" ] && [ "$top" != "$PWD" ] && [ -f "$top/shapeware/index.bsh" ]; then
  impl=$top
else
  impl=$PWD/.shapeware/src
  [ -d "$impl/.git" ] || git clone -q https://github.com/qiangli/outpost.git "$impl"
  git -C "$impl" checkout -q --detach 791fd29dac401492acfe597875846073bc48d33e
fi
printf '%s\n' "$impl" > .shapeware/impl
printf 'implementation: %s\n' "$impl"
```

### build
Build outpost from the resolved implementation or supplied work copy, on the gate host, the way `scripts/build.sh` does (tag `fb_archives`, no cgo), into the implementation's `bin/outpost`.
Effects: read, write, exec, net, priv
Band: unmeasured

```bash
set -eu
impl=${IMPL:-$(cat .shapeware/impl)}
mkdir -p "$impl/bin"
(cd "$impl" && GOWORK=off CGO_ENABLED=0 go build -tags fb_archives -trimpath -o bin/outpost ./cmd/outpost)
"$impl/bin/outpost" version 2>&1 | head -1
```

### test
Run the loopback probe against the built implementation.
Effects: read, write, exec
Band: unmeasured

```bash
set -eu
impl=${IMPL:-$(cat .shapeware/impl)}
python3 probe.py "$impl/bin/outpost" examples/grants.json
```

### install
No installation is performed by this draft.
Effects: read
Band: none

```bash
printf '%s\n' 'install: no installation defined'
```

### run
Show reference CLI help from the built implementation.
Effects: read, exec
Band: unmeasured

```bash
set -eu
impl=${IMPL:-$(cat .shapeware/impl)}
"$impl/bin/outpost" apps --help
```

### check
Static lint of this folder and, inside the repository, of every surface (the format §5b).
Effects: read
Band: none

```bash
set -eu
bashy conform spec check .
top=$(git rev-parse --show-toplevel 2>/dev/null || true)
if [ -n "$top" ] && [ -f "$top/shapeware/index.bsh" ]; then (cd "$top" && bashy --bashsharp shapeware/index.bsh check); fi
```

### cert
Certify the built implementation (3 runs, fresh HOME). `CANDIDATE=<outpost binary>` evaluates another implementation against the reference. On the gate host.
Effects: read, write, exec, net
Band: none

```bash
set -eu
impl=${IMPL:-$(cat .shapeware/impl)}
ref="$impl/bin/outpost"
mkdir -p evidence
bashy conform cert --contract CONTRACT.md --reference-cmd "$ref" --candidate-cmd "${CANDIDATE:-$ref}" --bin outpost --runs 3 --dir "$(pwd -P)" --fresh-home --timeout 120s --out evidence/cert.json
```

### mutate
Mutation-test the contract against the built implementation: ten assertions, about twenty mutants at about one probe run each (minutes). On the gate host.
Effects: read, write, exec, net
Band: none

```bash
set -eu
impl=${IMPL:-$(cat .shapeware/impl)}
mkdir -p evidence
bashy conform mutate CONTRACT.md --path "$impl/bin" --dir "$(pwd -P)" --fresh-home --timeout 120s | tee evidence/mutate.log
```

### alone
The mechanical half of the folder-alone check: copy this folder outside any checkout and run its own source, build and test there. On the gate host.
Effects: read, write, exec, net, priv
Band: none

```bash
set -eu
tmp=$(mktemp -d)
cp -R "$(pwd -P)" "$tmp/member-access"
rm -rf "$tmp/member-access/.shapeware" "$tmp/member-access/evidence"
cd "$tmp/member-access"
bashy dag -f dag.md source && bashy dag -f dag.md build && bashy dag -f dag.md test
```
