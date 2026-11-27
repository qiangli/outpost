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
Build in the resolved implementation or supplied work copy, on the gate host.
Effects: read, write, exec
Band: unmeasured

```bash
set -eu
impl=${IMPL:-$(cat .shapeware/impl)}
(cd "$impl" && GOWORK=off ./scripts/build.sh)
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
