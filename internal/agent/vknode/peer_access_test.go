package vknode

import "testing"

// TestPeerNamespacePolicy_FailClosed is the core of the peer-plane admission
// fix: without a cloudbox FetchAccess, the gate must DENY by default, not fall
// through to the nil "allow all" behavior.
func TestPeerNamespacePolicy_FailClosed(t *testing.T) {
	// Empty policy → every namespace denied (fail closed), and the gate is
	// non-nil so Allowed actually runs the check.
	empty := PeerNamespacePolicy(nil)
	if empty == nil {
		t.Fatalf("peer policy must be non-nil even for an empty list (else it fails OPEN)")
	}
	if empty.Allowed("user-abc") {
		t.Fatalf("empty peer policy must deny all namespaces")
	}
	if empty.Allowed("default") {
		t.Fatalf("empty peer policy must deny 'default' too")
	}

	// Declared policy admits only the listed namespaces.
	pol := PeerNamespacePolicy([]string{"user-abc", "team-x"})
	if !pol.Allowed("user-abc") || !pol.Allowed("team-x") {
		t.Fatalf("declared namespaces should be allowed")
	}
	if pol.Allowed("user-zzz") {
		t.Fatalf("undeclared namespace must be denied under a peer policy")
	}
}

// TestPeerNamespacePolicy_ContrastWithNil documents the exact gap the fix
// closes: a bare nil *Access allows everything, which is why a peer node must
// never be handed nil.
func TestPeerNamespacePolicy_ContrastWithNil(t *testing.T) {
	var failOpen *Access // the pre-fix peer path
	if !failOpen.Allowed("anything") {
		t.Fatalf("nil *Access is fail-open by contract (this is what peer join must NOT use)")
	}
	failClosed := PeerNamespacePolicy(nil)
	if failClosed.Allowed("anything") {
		t.Fatalf("PeerNamespacePolicy(nil) must be fail-closed")
	}
}
