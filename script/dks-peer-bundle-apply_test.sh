#!/usr/bin/env bash
# Offline tests for script/dks-peer-bundle-apply.sh.
# Needs NO cluster, NO kubectl, NO network, NO go build. Run:
#     bash script/dks-peer-bundle-apply_test.sh
#
# Two layers, same discipline as script/dks-peer-acceptance_test.sh:
#   1. unit tests of the pure helpers (sourced with DKS_BUNDLE_APPLY_LIB_ONLY=1);
#   2. behavioral tests that execute the REAL script against a STUB runner
#      on a minimal env and assert its exit status, its messages, and the
#      exact args the stub received. Nothing simulates the script's logic
#      by hand — every invariant is observed on the running script itself.
set -uo pipefail

# $0 rather than BASH_SOURCE: this file is always executed, never sourced,
# and BASH_SOURCE is not populated by every bash-compatible runner.
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/dks-peer-bundle-apply.sh"

T_PASS=0; T_FAIL=0
ok()  { T_PASS=$((T_PASS+1)); echo "ok   - $1"; }
bad() { T_FAIL=$((T_FAIL+1)); echo "FAIL - $1"; [ -z "${2:-}" ] || echo "       $2"; return 0; }
is()  { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$3] got [$2]"; fi; }
contains() { case "$2" in *"$3"*) ok "$1" ;; *) bad "$1" "output missing [$3]: $2" ;; esac; }

FIX="$(mktemp -d)"
trap 'rm -rf "$FIX"' EXIT

# ---------------------------------------------------------------------------
# Layer 1 — pure helpers (sourced; nothing executes)
# ---------------------------------------------------------------------------
DKS_PEER_KUBECONFIG="$FIX/peer/k3s.yaml"
DKS_CLOUDBOX_KUBECONFIG="$FIX/cloud/outpost.yaml"
export DKS_PEER_KUBECONFIG DKS_CLOUDBOX_KUBECONFIG
DKS_BUNDLE_APPLY_LIB_ONLY=1 . "$SCRIPT"
set +e  # the script enables `set -e`; do not let it kill the test shell

# expand_tilde
is "expand_tilde ~ -> HOME"        "$(expand_tilde '~')"          "$HOME"
is "expand_tilde ~/x -> HOME/x"    "$(expand_tilde '~/x')"        "$HOME/x"
is "expand_tilde /abs passthrough" "$(expand_tilde '/abs/path')"  "/abs/path"

# resolve_kubeconfig: --peer picks the peer path
out="$(resolve_kubeconfig 1 "")"; rc=$?
is  "resolve --peer -> rc 0" "$rc" "0"
is  "resolve --peer -> peer path" "$out" "$DKS_PEER_KUBECONFIG"

# resolve_kubeconfig: explicit path passes through (tilde expanded)
out="$(resolve_kubeconfig 0 "/tmp/some/k3s.yaml")"; rc=$?
is  "resolve explicit -> rc 0" "$rc" "0"
is  "resolve explicit -> path" "$out" "/tmp/some/k3s.yaml"

# resolve_kubeconfig: neither source -> fail, no default cluster
out="$(resolve_kubeconfig 0 "" 2>&1)"; rc=$?
is       "resolve neither -> rc 1" "$rc" "1"
contains "resolve neither -> names the miss" "$out" "no default cluster"

# resolve_kubeconfig: both --peer and explicit -> mutually exclusive
out="$(resolve_kubeconfig 1 "/tmp/k.yaml" 2>&1)"; rc=$?
is       "resolve both -> rc 1" "$rc" "1"
contains "resolve both -> mutually exclusive" "$out" "mutually exclusive"

# resolve_kubeconfig: refuse the cloudbox kubeconfig (the core invariant)
out="$(resolve_kubeconfig 0 "$DKS_CLOUDBOX_KUBECONFIG" 2>&1)"; rc=$?
is       "resolve cloudbox -> rc 1" "$rc" "1"
contains "resolve cloudbox -> refuses conflation" "$out" "refusing to apply a PEER bundle against the cloudbox"

# ---------------------------------------------------------------------------
# Layer 2 — behavioral (execute the real script against a stub runner)
# ---------------------------------------------------------------------------

# A stub that records the args it was invoked with and exits with $STUB_RC.
STUB="$FIX/stub.sh"
cat >"$STUB" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$STUB_ARGS_FILE"
exit "${STUB_RC:-0}"
EOF
chmod +x "$STUB"

# Seed real files so the existence checks pass on the happy path.
mkdir -p "$FIX/peer" "$FIX/cloud"
echo "apiVersion: v1" > "$DKS_PEER_KUBECONFIG"
echo "apiVersion: v1" > "$DKS_CLOUDBOX_KUBECONFIG"
BUNDLE="$FIX/bundle.yaml"
echo "kind: Namespace" > "$BUNDLE"

STUB_ARGS_FILE="$FIX/args.txt"
export STUB_ARGS_FILE

# run <expected-rc> <label> -- <script args...>
# Executes the real script with the stub runner; asserts exit code.
run_script() {
    DKS_PEER_KUBECONFIG="$DKS_PEER_KUBECONFIG" \
    DKS_CLOUDBOX_KUBECONFIG="$DKS_CLOUDBOX_KUBECONFIG" \
    DKS_BUNDLE_APPLY_CMD="bash $STUB" \
    STUB_ARGS_FILE="$STUB_ARGS_FILE" \
    STUB_RC="${STUB_RC:-0}" \
    bash "$SCRIPT" "$@" 2>"$FIX/stderr.txt"
}

# --- happy path: --peer reaches the runner with the peer kubeconfig -------
: > "$STUB_ARGS_FILE"
run_script --peer --bundle "$BUNDLE"; rc=$?
is "happy --peer -> rc 0" "$rc" "0"
args="$(cat "$STUB_ARGS_FILE")"
contains "happy --peer -> runner got --kubeconfig" "$args" "--kubeconfig"
contains "happy --peer -> runner got peer path"    "$args" "$DKS_PEER_KUBECONFIG"
contains "happy --peer -> runner got --bundle"     "$args" "$BUNDLE"

# --- happy path: explicit --kubeconfig ------------------------------------
: > "$STUB_ARGS_FILE"
run_script --kubeconfig "$DKS_PEER_KUBECONFIG" --bundle "$BUNDLE"; rc=$?
is "happy explicit -> rc 0" "$rc" "0"

# --- runner failure propagates (exec => script exit == runner exit) -------
STUB_RC=7 run_script --peer --bundle "$BUNDLE"; rc=$?
is "runner failure propagates -> rc 7" "$rc" "7"

# --- failure: unknown flag ------------------------------------------------
run_script --peer --bogus --bundle "$BUNDLE"; rc=$?
is       "unknown flag -> rc 2" "$rc" "2"
contains "unknown flag -> loud" "$(cat "$FIX/stderr.txt")" "unknown flag"

# --- failure: missing --bundle --------------------------------------------
run_script --peer; rc=$?
is       "missing bundle -> rc 2" "$rc" "2"
contains "missing bundle -> loud" "$(cat "$FIX/stderr.txt")" "--bundle is required"

# --- failure: no kubeconfig source (no default cluster) -------------------
run_script --bundle "$BUNDLE"; rc=$?
is       "no kubeconfig -> rc 2" "$rc" "2"
contains "no kubeconfig -> loud" "$(cat "$FIX/stderr.txt")" "no default cluster"

# --- failure: peer + explicit are mutually exclusive ----------------------
run_script --peer --kubeconfig "$DKS_PEER_KUBECONFIG" --bundle "$BUNDLE"; rc=$?
is       "peer+explicit -> rc 2" "$rc" "2"
contains "peer+explicit -> loud" "$(cat "$FIX/stderr.txt")" "mutually exclusive"

# --- failure: cloudbox kubeconfig is refused ------------------------------
run_script --kubeconfig "$DKS_CLOUDBOX_KUBECONFIG" --bundle "$BUNDLE"; rc=$?
is       "cloudbox refused -> rc 2" "$rc" "2"
contains "cloudbox refused -> loud" "$(cat "$FIX/stderr.txt")" "refusing to apply a PEER bundle"

# --- failure: kubeconfig file missing (evidence invariant) ----------------
run_script --kubeconfig "$FIX/nope.yaml" --bundle "$BUNDLE"; rc=$?
is       "missing kubeconfig file -> rc 2" "$rc" "2"
contains "missing kubeconfig file -> loud" "$(cat "$FIX/stderr.txt")" "kubeconfig not found"

# --- failure: bundle path missing (evidence invariant) --------------------
run_script --peer --bundle "$FIX/no-bundle"; rc=$?
is       "missing bundle path -> rc 2" "$rc" "2"
contains "missing bundle path -> loud" "$(cat "$FIX/stderr.txt")" "bundle path not found"

# ---------------------------------------------------------------------------
echo
echo "dks-peer-bundle-apply_test: $T_PASS passed, $T_FAIL failed"
[ "$T_FAIL" -eq 0 ]
