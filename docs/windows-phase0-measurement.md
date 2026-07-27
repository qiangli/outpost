# Windows Phase 0 measurement

Measured 2026-07-27 from a Darwin/arm64 host with Go 1.26.5. This is an
evidence report, not an implementation plan. No defect found during this pass
was fixed.

The measured revisions were:

```text
$ git rev-parse HEAD
ddd90f526d5f5673da1640e6e3840a67422f0223
$ git -C ../coreutils rev-parse HEAD
cda53047f947f17679789b9e0f63d8e1e635ce10
$ git -C ../sh rev-parse HEAD
ed094a326217c50505f0caf08971a96a0c316d42
$ go version
go version go1.26.5 darwin/arm64
```

## Result summary

| area | what was measured | result | evidence (exact command and output) |
|---|---|---|---|
| outpost | Windows/amd64 production-package compile | **PASS** | `$ GOOS=windows GOARCH=amd64 go build ./...`<br>`[no output; exit 0]` |
| outpost | Windows/amd64 vet, including test packages | **PASS** | `$ GOOS=windows GOARCH=amd64 go vet ./...`<br>`[no output; exit 0]` |
| coreutils | Windows/amd64 whole-repository compile | **BLOCKED/FAIL** before compilation: the provisioned sibling has an unmaterialized `external/ollama/src` submodule. This is not a Windows source verdict. All 20 reporting import sites are named under [Raw cross-compile failure](#raw-cross-compile-failure). | `$ (cd ../coreutils && GOOS=windows GOARCH=amd64 go build ./...)`<br>Exit 1; exact output below. |
| coreutils | Windows/amd64 whole-repository vet | **BLOCKED/FAIL** for the same missing replacement module. No pass is claimed for packages vet never reached. | `$ (cd ../coreutils && GOOS=windows GOARCH=amd64 go vet ./...)`<br>Exit 1; output was byte-for-byte identical to the build output below. |
| coreutils weave | The motivating test/build-tag path, isolated from the missing Ollama submodule | **PASS** | `$ (cd ../coreutils && GOOS=windows GOARCH=amd64 go build ./pkg/weave)`<br>`[no output; exit 0]`<br>`$ (cd ../coreutils && GOOS=windows GOARCH=amd64 go vet ./pkg/weave)`<br>`[no output; exit 0]` |
| coreutils steward | The Windows `LockFileEx` reference implementation | **PASS** | `$ (cd ../coreutils && GOOS=windows GOARCH=amd64 go build ./pkg/steward)`<br>`[no output; exit 0]`<br>`$ (cd ../coreutils && GOOS=windows GOARCH=amd64 go vet ./pkg/steward)`<br>`[no output; exit 0]` |
| coreutils submodules | Whether the whole-repository failure was caused by an absent provisioned dependency | **MISSING**: both registered source submodules are absent (`-` prefix); only Ollama blocks these commands. | `$ git -C ../coreutils submodule status`<br>`-db9cd0a2004b53534cd84a2947063cd73de97123 external/ollama/src`<br>`-d454baa0afecfcf69057ab16735579f7b9da033b external/podman/src` |
| bashy | Reachable sibling clone | **NOT MEASURED**: no `../bashy` clone was provisioned. The installed executable is not a repository and was outside the requested sibling-clone evidence path. | `$ find .. -maxdepth 2 -type d -name bashy`<br>`[no output; exit 0]` |
| ycode | Reachable sibling clone | **NOT MEASURED**: no `../ycode` clone was provisioned. | `$ find .. -maxdepth 2 -type d -name ycode`<br>`[no output; exit 0]` |
| existing uniformity list | Requested umbrella reconciliation input | **NOT AVAILABLE** in this checkout, provisioned siblings, or their local Git histories. A faithful item-by-item reconciliation is therefore impossible. The local Windows TODO evidence is reconciled below instead. | `$ find . .. -maxdepth 4 -type f -name 'windows-crossplatform-uniformity.md' -o -name 'per-os-build-test-over-dks-plan.md'`<br>`[no output; exit 0]`<br>`$ git log --all --oneline -- docs/windows-crossplatform-uniformity.md docs/per-os-build-test-over-dks-plan.md`<br>`[no output; exit 0]`<br>`$ git -C ../coreutils log --all --oneline -- docs/windows-crossplatform-uniformity.md docs/per-os-build-test-over-dks-plan.md`<br>`[no output; exit 0]` |

The top-line result is therefore: outpost is Windows-clean at compile/vet time;
the coreutils `weave` and `steward` paths are Windows-clean in isolation; the
whole coreutils verdict is unknown because the clone is incomplete. Compilation
does not make the silent behavioral gaps below passes.

## Raw cross-compile failure

Both whole-coreutils commands returned the following output. The single root
failure is the absent replacement module
`external/ollama/src/go.mod`; it is emitted at these 20 import sites, all named
here rather than collapsed into “dependency error”:

```text
external/ollama/ollama_component.go:25:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
external/ollama/ollama_component.go:26:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/bindings/bindings.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/bindings/containers/containers.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/bindings/images/images.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/bindings/network/network.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/bindings/pods/pods.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/bindings/system/system.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/entities/entities.go:5:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/entities/entities.go:6:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/handlers/handlers.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/machine/machine.go:5:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/machine/machine.go:6:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/machine/machine.go:7:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/machine/machine.go:8:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/machine/machine.go:9:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/machine/machine.go:10:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/oci/specgen/specgen.go:4:8: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/ollm/ollm.go:14:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
pkg/ollm/ollm.go:15:2: github.com/ollama/ollama@v0.0.0-00010101000000-000000000000 (replaced by ./external/ollama/src): reading external/ollama/src/go.mod: open /Users/qiangli/.bashy/weave/outpost-06baad9c/workspaces/coreutils/external/ollama/src/go.mod: no such file or directory
```

The clone was deliberately not repaired or initialized: this phase measures the
provisioned path as found, and the task forbids mixing fixes with measurement.

## Windows silent-stub and no-op inventory

Classification here is about the caller-visible result, not whether a comment
admits the limitation.

| path | what it should do | Windows behavior measured from source | failure mode |
|---|---|---|---|
| `../coreutils/pkg/weave/weave_lock_windows.go` | Serialize queue read-modify-write operations and honor bounded wait. | Calls load/mutate/save with no OS mutex and ignores `wait`. Concurrent writers can reuse IDs, overwrite one another, or expose inconsistent lifecycle state. | **Silent**; normal success is returned unless the unlocked I/O itself errors. |
| `../coreutils/pkg/weave/weave_lock_windows.go` | Exclude concurrent pulls via `pull.lock`. | Ignores `dir` and invokes `fn` immediately. | **Silent**; two pulls can run together. |
| `../coreutils/pkg/weave/weave_cooldown_lock_windows.go` | Serialize cooldown read-modify-write. | Loads/mutates/saves without a lock. | **Silent**; a concurrent cooldown update can be lost. |
| `../coreutils/pkg/policy/coord/lock_windows.go` | Serialize claim acquisition so exactly one agent can win. | Creates the directory and calls the critical section without a lock. | **Silent**; simultaneous claimants can both conclude the project is free. |
| `../coreutils/pkg/weave/weave_setsid_windows.go` | Detach the wrapper, detect an existing live wrapper, and stop a wrapper tree. | `weaveMaybeSetsid` and `weaveStopWrapper` do nothing; `pidAlive` always returns false. | **Silent**; duplicate-wrapper detection degrades to “not running,” and abandon/stop can leave the wrapper alive. |
| `../coreutils/pkg/weave/weave_heartbeat_windows.go` | Run heartbeat in background daemon mode when requested. | Prints a warning and runs in the foreground. | **Loud degradation**, not silent: stderr says background mode is unsupported. |
| `../coreutils/pkg/agentpty/pty_windows.go` | Give agent runs a PTY, trust-prompt clearing, and steering. | `Supported` is false; direct `Run` returns an explicit unsupported error. Callers may fall back to plain exec. | **Loud if called directly**; fallback is functional but loses steering/trust clearing. |
| `../coreutils/pkg/jobs/jobs_windows.go` | Shell job listing/control. | Operations return explicit “not available on Windows” errors. | **Loud**, not a silent stub. |
| `../coreutils/pkg/ask/owner_windows.go` | Verify that a rendezvous artifact is owned by the current account. | `checkOwner` returns nil; protection is delegated to the profile directory ACL, which this function does not inspect. | **Silent security asymmetry**; whether directory ACLs are sufficient is `[needs-win]`. |
| `internal/agent/shell/pty_windows.go` and `internal/agent/shell/vpty.go` | Provide interactive terminal semantics. | A pipe-backed virtual PTY works, including stored geometry. It intentionally has no SIGWINCH, `$COLUMNS`/`$LINES` updates, foreground-input echo, terminal identity (`tty` sees a pipe), or signal-generating Ctrl-C. | **Silent functional degradation** for terminal-dependent programs; direct resizing of a non-virtual master fails loudly. |
| `internal/agent/sshclient/sigwinch_windows.go` | Forward local terminal resize to a remote SSH PTY. | `sigwinch` is nil, so the resize goroutine receives no Windows resize event. | **Silent**; the remote PTY retains its old size. |
| `internal/agent/vknode/backend_ollama_windows.go` (`killProcessTree`) | Terminate the complete native workload tree and report failure. | Runs `taskkill /T /F`, discards every error, and always returns nil. | **Silent**; `Delete` can forget the registry row and report success while the process/tree remains alive. **[needs-win]** to exercise taskkill denial/race cases. |
| `internal/agent/vknode/backend_ollama_windows.go` (`processAlive`) | Distinguish a live process from a missing one. | Uses `OpenProcess` plus `GetExitCodeProcess(STILL_ACTIVE)`, which is a real implementation. Any handle-open error, including access denial, is collapsed to false. | Normally functional; **silent false-negative** when a live PID cannot be queried. **[needs-win]** for ACL-boundary behavior. |
| `internal/agent/sysinfo/sysinfo_windows.go` | Report host memory, disk, CPU model, and GPU data. | Memory/disk use kernel32 and are substantive. PowerShell/CIM failures and parse failures become empty CPU/GPU fields; kernel API failures become zero values. | **Silent best-effort omission**. OS, arch, CPU count, and usually hostname remain populated by common code. **[needs-win]** for real values. |
| `internal/agent/runtime/image/cni/internal/plugin/netlink_other.go` | Configure or remove pod networking if the CNI binary is invoked. | `EnsureBridge` and `PlugPod` return `errNotLinux`; `UnplugPod` returns nil without cleanup. The intended runtime image compiles and invokes this CNI on Linux, even when its host is Windows. | Add/setup **fail loudly** if mis-invoked on Windows; delete **silently no-ops**. Not on the intended Windows-host execution path. |
| `cmd/outpost/main.go` (`processAlive`; no Windows sibling) | Detect the daemon/supervisor for singleton enforcement, status, restart, and stop. | Uses `p.Signal(syscall.Signal(0))`, a POSIX probe. The checked-in Windows TODO records that it always returns false on Windows and includes a 2026-07-22 observation from host `puppy`. | **Silent and critical**: live pidfiles are treated as stale, duplicate daemons are allowed, status lies, and `outpost stop` removes the pidfile without stopping the process. Still open in this revision. |

The command used to locate the Windows-specific candidates was:

```text
$ rg --files . | rg '(_windows\.go$|windows.*\.go$)' | sort
./cmd/outpost/detach_windows.go
./cmd/outpost/service_windows.go
./internal/agent/hostauth/hostauth_windows.go
./internal/agent/localsock/pipe_windows.go
./internal/agent/osversion/osversion_windows.go
./internal/agent/shell/pty_windows.go
./internal/agent/sshclient/sigwinch_windows.go
./internal/agent/supervisor/signal_windows.go
./internal/agent/sysinfo/sysinfo_windows.go
./internal/agent/sysload/sample_windows.go
./internal/agent/upgrade/swap_windows.go
./internal/agent/vknode/backend_ollama_windows.go
```

The other outpost Windows siblings contain substantive implementations rather
than do-nothing compatibility bodies: daemon detach/Task Scheduler service,
`LogonUserW` authentication, named-pipe transport, OS version, hard process
kill, system-load kernel probes, and executable swap. Cross-compilation proves
their type/link compatibility only; runtime validation remains `[needs-win]`.

The coreutils lock/no-op evidence was produced by:

```text
$ rg -n "withWeaveQueueLock|withWeavePullLock|withWeaveCooldownLock|weaveMaybeSetsid|pidAlive|weaveStopWrapper|best-effort|no-op" ../coreutils/pkg/weave/*windows.go ../coreutils/pkg/policy/coord/lock_windows.go
../coreutils/pkg/policy/coord/lock_windows.go:13:// withLock on Windows is best-effort: there is no flock, and weave's queue lock has
../coreutils/pkg/weave/weave_setsid_windows.go:5:// weaveMaybeSetsid is a no-op on Windows; we don't have setsid
../coreutils/pkg/weave/weave_setsid_windows.go:9:func weaveMaybeSetsid(parentStdinTTY bool) {}
../coreutils/pkg/weave/weave_setsid_windows.go:11:// pidAlive on Windows always reports false — the weave wrapper
../coreutils/pkg/weave/weave_setsid_windows.go:14:func pidAlive(pid int) bool { return false }
../coreutils/pkg/weave/weave_setsid_windows.go:16:// weaveStopWrapper on Windows is unimplemented for the MVP — the
../coreutils/pkg/weave/weave_setsid_windows.go:20:func weaveStopWrapper(pid int) {}
../coreutils/pkg/weave/weave_lock_windows.go:10:// withWeaveQueueLockWait on Windows is best-effort: we simply load, mutate,
../coreutils/pkg/weave/weave_lock_windows.go:15:func withWeaveQueueLockWait(dir string, wait time.Duration, fn func(*weaveQueue) error) error {
../coreutils/pkg/weave/weave_lock_windows.go:27:func withWeaveQueueLock(dir string, fn func(*weaveQueue) error) error {
../coreutils/pkg/weave/weave_lock_windows.go:28:	return withWeaveQueueLockWait(dir, weaveQueueLockWait, fn)
../coreutils/pkg/weave/weave_lock_windows.go:31:// withWeavePullLock has no OS-level mutex on Windows, matching the queue lock
../coreutils/pkg/weave/weave_lock_windows.go:33:func withWeavePullLock(dir string, fn func() error) error {
../coreutils/pkg/weave/weave_cooldown_lock_windows.go:5:// withWeaveCooldownLock on Windows is best-effort (no OS-level mutex),
../coreutils/pkg/weave/weave_cooldown_lock_windows.go:6:// mirroring withWeaveQueueLock. Cooldown writes are themselves best-effort,
../coreutils/pkg/weave/weave_cooldown_lock_windows.go:8:func withWeaveCooldownLock(dir string, fn func(*toolCooldowns) error) error {
```

`../coreutils/pkg/steward/lock_windows.go` is the counterexample: it takes a real
exclusive whole-file `LockFileEx` lock and its isolated package build/vet passes.
That confirms the known weave/coord gaps are not forced by the platform.

## Can a vknode register and remain Ready on Windows?

### What can be determined from code

- **Registration path:** `vknode.Run`, the client-go informers, virtual-kubelet
  controllers, token-file code, and Provider/runner code are untagged and
  Windows-cross-compile successfully. No Unix-only API was found in the
  Provider/runner path.
- **Node identity and capacity:** `sysinfo.Collect` always supplies compile-time
  `OS`, `Arch`, and runtime `CPUCount`. `BuildNodeFromInfo` therefore gets usable
  Windows OS/arch, CPU capacity, and the fixed pod capacity even if every
  Windows-specific probe fails. `GlobalMemoryStatusEx` supplies physical memory;
  on failure the node advertises zero memory rather than refusing registration.
  Disk is not used by `BuildNode` because it calls `Collect("")`.
- **Native launch:** `defaultLaunch` is not a stub. It uses
  `CREATE_NEW_PROCESS_GROUP`, redirects logs when it can open the file, starts
  the process, and reaps it asynchronously.
- **Native alive:** `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` plus
  `GetExitCodeProcess == STILL_ACTIVE` is a real Windows liveness probe.
- **Native terminate:** structurally present via `taskkill /T /F`, but incomplete
  as an accountable operation because every execution error is discarded.
- **Readiness caveat:** for a native backend, `NewNodeProvider(nil, node)` installs
  a pinger that always returns nil, and `Run` does not replace it. Its status loop
  repeatedly forces `NodeReady=True`. Consequently a Windows native vknode can
  *report* and remain Ready even if PowerShell probes fail, no workload binary
  can launch, or `taskkill` cannot terminate workloads. Ready is not operational
  evidence.
- **Podman-backed vk:** socket normalization, named-pipe dialing, enumeration of
  `\\.\pipe\podman-*`, and the `%TEMP%\podman\*-api.sock` candidate are
  implemented. The client uses the versioned libpod HTTP API and the node pinger
  actually calls libpod `_ping`.
- **Agent/runtime mode:** the host-side supervisor cross-compiles and intentionally
  runs a privileged Linux k3s-agent container. Its CNI is compiled for Linux in
  the image. Whether the WSL-backed Podman machine accepts the exact privileged,
  cgroup namespace, `/lib/modules`, named-volume, networking, and GPU behavior
  cannot be derived from a Darwin cross-build.

### Verdict

The code is sufficient to say that a Windows vknode has a plausible,
non-Unix-only registration path and that the Windows native process backend is
substantive rather than a placeholder. It is **not** sufficient to say that a
node actually registers, runs a workload, and survives a lifecycle on Windows.

- **[needs-win]** `vk-native`/`vk-ollama`: register against an apiserver; verify
  Node labels/capacity; launch a listening process; observe Pod Ready; restart
  outpost and adopt it; delete it; prove the full process tree is gone.
- **[needs-win]** `vk-podman`: start/discover the Podman machine through both
  named-pipe and AF_UNIX candidates; `_ping`; create/status/delete a pod; verify
  the Node becomes NotReady when the daemon disappears.
- **[needs-win]** `agent`: build/start the privileged runtime in the WSL-backed
  Podman machine, join k3s, exercise CNI/service networking, and restart/rejoin.
- **[needs-win]** shell: interactive editing, resize, Ctrl-C, and native child
  stdin behavior through both Matrix shell and SSH.

## Reconciliation with existing lists

The requested `docs/windows-crossplatform-uniformity.md` and
`docs/per-os-build-test-over-dks-plan.md` do not exist in the supplied outpost
checkout, the provisioned sibling clones, or their local Git histories. This
report therefore does **not** create a competing status list or pretend to have
reconciled unseen `[needs-win]` entries. Once the umbrella document is supplied,
the findings above should be folded into it and this file retained as the raw
Phase-0 evidence.

The existing material that *is* present reconciles as follows:

| existing item | measured status |
|---|---|
| Motivating `pkg/weave` tests referenced Unix-only `weaveFlock` without a build tag | **FIXED in this checkout**: the concurrency test has `//go:build !windows`, a Windows test sibling exists, and Windows build/vet of `./pkg/weave` pass. This does not fix locking behavior. |
| Windows weave queue/pull locking is a no-op; steward `LockFileEx` is the drop-in pattern | **STILL OPEN**, confirmed directly in `weave_lock_windows.go`; cooldown and policy/coord locking have the same silent shape. |
| `docs/todo/576fcf3dcea2-processalive-is-unix-only-always-false-on-windows.md` | **STILL OPEN**: current `cmd/outpost/main.go` still has the untagged signal-0 implementation and no Windows sibling. |
| `docs/settings.md` says `agent`, `vk-native`, and `vk-podman` are all supported on Windows | **NOT PROVEN / documentation conflict**: the cross-build passes, but a checked-in TODO says the Windows WSL runtime is untested. All three runtime claims remain `[needs-win]` at this evidence level. |
| Windows native-process launch/alive/terminate implementation | **PARTLY FIXED/IMPLEMENTED**: launch and alive are real implementations. Terminate silently discards errors; end-to-end lifecycle remains `[needs-win]`. |

## Explicit measurement limits

This pass could not measure:

1. Any Windows runtime behavior, because no Windows host was available.
   Cross-compilation cannot validate Win32 calls, PowerShell/CIM output, ACLs,
   named pipes, Task Scheduler, taskkill, console semantics, or WSL/Podman.
2. Whole-coreutils Windows health beyond the named failure, because its
   provisioned Ollama (and Podman) source submodules were absent. They were not
   initialized because the task forbids following remotes and asks for the
   provisioned sibling state.
3. Bashy or ycode source health, because no sibling clone of either repository
   was reachable.
4. Item-by-item reconciliation against the requested umbrella list, because the
   file and the Phase-0 plan were absent from every supplied local checkout and
   local history.
5. Windows test execution. `GOOS=windows` build/vet on Darwin proves compilation
   and static analysis, not that Windows test binaries run. No test execution
   pass is claimed.
