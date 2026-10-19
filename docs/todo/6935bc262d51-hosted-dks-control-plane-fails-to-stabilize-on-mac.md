---
id: 6935bc262d51
kind: bug
title: 'Hosted DKS control plane fails to stabilize on macOS: metrics-server RBAC forbidden, then etcd peer IP mismatch on restart'
seq: 1897
status: todo
priority: p1
labels:
    - dks
created: 2026-10-19T02:17:18.290239Z
sprint: 454
sprint_id: 81c8d9ad-b9a7-5a08-9f9f-a4e5fed32098
sprint_title: 'Post-S443 must-fixes: cloudbox tier-5 plane green, zot config reapplied on every start, Bash# tour green'
---

Observed 2026-10-18 on a macOS test host (podman machine), Sprint 446 S18 live leg: a disposable hosted outpost DKS control plane started from a freshly built signed scratch runtime image. First container exited at startup because metrics-server bootstrap hit RBAC forbidden (bootstrap race); the restarted control plane then failed with an etcd peer IP mismatch (the container IP changed across the restart, from one podman bridge address to another) and /readyz stayed 503. A direct k3s from the SAME image ran fine (kubectl get pods -A ok). Expected: hosted DKS reaches ready on first start and survives a restart with a changed container IP.

2026-10-19 reconciliation: attached to Sprint 454; no duplicate bug. Blocks the remaining Sprint 450 S18 peer-hosted DKS locator proof. Fix belongs to known DKS defects, not new features.

Moved out (2026-10-19): the clause "same released Outpost must subsequently pass ordinary endpoint compatibility against standalone and satellite" belongs to Sprint 450 (satellite work). It is not part of closing this story.

Part 1: metrics-server supervisor must tolerate a transient forbidden (ready to implement).
- Confirmed at outpost d442d90 with a stub probe. internal/agent/runtime/image/metrics-server-supervisor.sh line 225 retries only kubectl apply, and nothing waits for the RBAC authorizer to see the new bindings.
- When metrics-server, started at line 276, exits once on a transient forbidden error, lines 296-301 exit 1. server-entrypoint.sh lines 258-272 then kill the whole control-plane container.
- Fix: before starting metrics-server, wait with a bounded loop until kubectl auth can-i get configmaps/extension-apiserver-authentication -n kube-system --as=system:serviceaccount:kube-system:metrics-server answers yes. Also retry with backoff when metrics-server exits early on forbidden, and only fail after a bounded budget.
- Add an env path seam for the /var/lib/rancher/... and /run/outpost-metrics-server paths so the script runs under a test (the probe needed sed).

Part 2: etcd advertise mismatch on restart (re-reproduce at HEAD before any fix).
- A reconcile already exists: commit 5f35bc9 (2026-10-16) added outpost etcd-peer (internal/runtimecmd/etcd_peer.go). It is called at server-entrypoint.sh line 194 after k3s is started at line 189, with 60 tries. ADVERTISE_ADDR comes from hostname -i at line 49.
- The failure was observed on 10-18, after that commit, but the outpost SHA of the 10-18 image was not recorded.
- First establish whether that image predated 5f35bc9: rebuild the runtime image at HEAD and re-run the restart with a changed container IP on the macOS test host.
- If HEAD still fails, identify the failing step (k3s exits or blocks before the local etcd client port answers, or the 60 s bound is too short), then write the red test and fix.
- If HEAD passes, record that and close this part with the evidence. Do not write a speculative fix.

Tests:
- Existing tests, green at HEAD: go test -run EtcdPeer ./internal/runtimecmd/ and go test -run 'PeerMetrics|ControlPlaneReports|DoesNotAdvertiseLoopback' ./internal/agent/runtime/.
- New red test (part 1), in internal/agent/runtime, following the TestControlPlaneReportsFailedChild style: TestMetricsSupervisorSurvivesTransientForbidden. It stubs kubectl and outpost, makes the first metrics-exec exit 1 with forbidden, and asserts the supervisor retries or waits instead of exiting 1.
- Conditional test (part 2), only after a HEAD repro shows the failing step: TestServerEntrypointReconcilesEtcdPeerOnChangedIP. It stubs k3s, hostname -i and outpost etcd-peer, and asserts the reconcile runs with the new IP.

Acceptance: hosted DKS reaches ready on first boot without the metrics RBAC race, and after a container restart with a changed IP without an etcd advertise mismatch. Authenticated kubectl reads and namespace cleanup are proven, not serving=true alone.

Gate:
- Unit: cd outpost && go test -short -count=1 ./internal/agent/runtime/ ./internal/runtimecmd/. Short unit tests may run on the development host; the full suite runs on the macOS test host.
- First red test: TestMetricsSupervisorSurvivesTransientForbidden.
- Live, on the macOS test host (podman machine; containers never on the development host):
  1. Build the runtime image at the candidate SHA and record the SHA.
  2. Start a disposable hosted DKS control plane.
  3. With the hosted k3s.yaml: kubectl get --raw /readyz, kubectl get pods -A, kubectl top nodes.
  4. Restart the container with a forced container-IP change and repeat step 3.
  5. Create and delete a test namespace and confirm the delete completes.
  6. Remove the container and image.
- Optional Linux confirmation on a Linux test host.
- Evidence: the sprint's shared evidence record. Record the image SHA, the commands, the raw outputs and the cleanup result.
