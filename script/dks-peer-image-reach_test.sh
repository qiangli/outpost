#!/usr/bin/env bash
# script/dks-peer-image-reach_test.sh — offline, deterministic test suite
# for peer image distribution reach verification (recipes, not blobs).
#
# Asserts all 6 accepted requirements in the shell layer:
#   - Requirement 2: DISTINCT targets, identity binding (Node, Ref, Want)
#   - Requirement 3: Rejection of duplicate and foreign evidence
#   - Requirement 4: Containerd content digest correlated with provenance (no stale ref, loud mismatch)
#   - Requirement 6: Deterministic, offline, cluster-free gate execution

set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
PEER_IMAGE_REACH_LIB_ONLY=1 source "$SCRIPT_DIR/dks-peer-image-reach.sh"

TEST_PASSED=0
TEST_FAILED=0

assert_contains() {
    local label="$1" got="$2" expected="$3"
    if [[ "$got" == *"$expected"* ]]; then
        echo "PASS: $label"
        TEST_PASSED=$((TEST_PASSED + 1))
    else
        echo "FAIL: $label — expected substring \"$expected\", got:"
        echo "  $got"
        TEST_FAILED=$((TEST_FAILED + 1))
    fi
}

echo "=== Peer Image Reach Offline Test Suite ==="

# 1. Distinct Targets Validation
out=$(reach_verify_distinct_nodes 2 "node-a-worker1 node-b-worker1" 2>&1 || true)
assert_contains "Distinct nodes PASS when targets are unique" "$out" "CHECK distinct-targets PASS"

out=$(reach_verify_distinct_nodes 2 "node-a-worker1 node-a-worker1" 2>&1 || true)
assert_contains "Duplicate node target rejected" "$out" "CHECK distinct-targets FAIL"

out=$(reach_verify_distinct_nodes 2 "" 2>&1 || true)
assert_contains "Unnamed node target rejected" "$out" "CHECK distinct-targets FAIL"

out=$(reach_verify_distinct_nodes 3 "node-a node-b" 2>&1 || true)
assert_contains "Below minimum required targets rejected" "$out" "CHECK distinct-targets FAIL"


# 2. Evidence Verification: Foreign & Replayed Evidence
VALID_RECIPE="sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
VALID_CONTENT="sha256:1111111111111111111111111111111111111111111111111111111111111111"
OTHER_CONTENT="sha256:2222222222222222222222222222222222222222222222222222222222222222"

CH_A="node-a|localhost/app:v1|$VALID_RECIPE|nonce123"

out=$(reach_eval_report "$CH_A" "node-c|localhost/app:v1|$VALID_RECIPE|nonce123|resident|$VALID_CONTENT|$VALID_CONTENT" 2>&1 || true)
assert_contains "Foreign node evidence rejected" "$out" "REJECT foreign node"

out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|$VALID_RECIPE|wrong-nonce|resident|$VALID_CONTENT|$VALID_CONTENT" 2>&1 || true)
assert_contains "Foreign nonce evidence rejected" "$out" "REJECT foreign evidence"

out=$(reach_eval_report "$CH_A" "node-a|localhost/wrong-app:v1|$VALID_RECIPE|nonce123|resident|$VALID_CONTENT|$VALID_CONTENT" 2>&1 || true)
assert_contains "Identity ref mismatch rejected" "$out" "REJECT identity mismatch"

out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|sha256:0000000000000000000000000000000000000000000000000000000000000000|nonce123|resident|$VALID_CONTENT|$VALID_CONTENT" 2>&1 || true)
assert_contains "Identity recipe mismatch rejected" "$out" "REJECT identity mismatch"


# 3. State & Digest Correlation Checks
out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|$VALID_RECIPE|nonce123|absent||" 2>&1 || true)
assert_contains "Absent image state fails" "$out" "FAIL image is absent"

out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|$VALID_RECIPE|nonce123|unknown||" 2>&1 || true)
assert_contains "Unknown digest state fails" "$out" "FAIL could not determine"

out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|$VALID_RECIPE|nonce123|resident|invalid-digest-format|$VALID_CONTENT" 2>&1 || true)
assert_contains "Invalid content digest format fails" "$out" "FAIL invalid content digest"

out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|$VALID_RECIPE|nonce123|resident|$VALID_CONTENT|" 2>&1 || true)
assert_contains "Missing provenance fails" "$out" "FAIL no local provenance"

out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|$VALID_RECIPE|nonce123|resident|$VALID_CONTENT|$OTHER_CONTENT" 2>&1 || true)
assert_contains "Digest mismatch fails loudly" "$out" "FAIL digest mismatch"


# 4. Valid Evidence Case
out=$(reach_eval_report "$CH_A" "node-a|localhost/app:v1|$VALID_RECIPE|nonce123|resident|$VALID_CONTENT|$VALID_CONTENT" 2>&1 || true)
assert_contains "Valid evidence accepted" "$out" "ACCEPTED"


echo "-------------------------------------------"
echo "Results: $TEST_PASSED passed, $TEST_FAILED failed"

if [ "$TEST_FAILED" -gt 0 ]; then
    exit 1
fi
exit 0
