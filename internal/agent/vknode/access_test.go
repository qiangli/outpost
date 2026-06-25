package vknode

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAccess_AllowedAndSet(t *testing.T) {
	a := NewAccess("user-aaa", "user-bbb")
	if !a.Allowed("user-aaa") {
		t.Errorf("user-aaa should be allowed")
	}
	if a.Allowed("user-ccc") {
		t.Errorf("user-ccc should NOT be allowed")
	}

	// Set replaces atomically — old entries gone, new entries present.
	a.Set("user-ccc", "user-ddd")
	if a.Allowed("user-aaa") {
		t.Errorf("after Set, user-aaa should no longer be allowed")
	}
	if !a.Allowed("user-ccc") {
		t.Errorf("after Set, user-ccc should be allowed")
	}
}

func TestAccess_NilAllowsAll(t *testing.T) {
	var a *Access
	if !a.Allowed("anything") {
		t.Errorf("nil Access should allow every namespace (dev/single-tenant escape hatch)")
	}
	a.Set("user-x") // must not panic
	if got := a.Snapshot(); got != nil {
		t.Errorf("nil Access Snapshot should return nil; got %v", got)
	}
}

func TestAccess_EmptyTrimmedNamespacesSkipped(t *testing.T) {
	a := NewAccess("user-aaa", "", "   ", "user-bbb")
	if got := a.Snapshot(); len(got) != 2 {
		t.Errorf("Snapshot should have 2 entries (whitespace skipped), got %d: %+v", len(got), got)
	}
}

func TestNamespaceForEmail_DeterministicCaseAndWhitespace(t *testing.T) {
	cases := []struct{ a, b string }{
		{"alice@example.com", "ALICE@example.com"},
		{"alice@example.com", "  alice@example.com\t"},
		{"alice@example.com", "Alice@Example.COM"},
	}
	for _, tc := range cases {
		if NamespaceForEmail(tc.a) != NamespaceForEmail(tc.b) {
			t.Errorf("NamespaceForEmail should treat %q and %q the same", tc.a, tc.b)
		}
	}
}

func TestNamespaceForEmail_MatchesIndependentOracle(t *testing.T) {
	// Independent re-implementation of the cloudbox formula
	// (cluster.userNamespace -> "user-" + cluster.userSAName[2:],
	// userSAName -> "u-" + sha256(lower(trim(email)))[:6] hex).
	// If this oracle drifts the test screams.
	oracle := func(email string) string {
		h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
		return "user-" + hex.EncodeToString(h[:6])
	}
	for _, email := range []string{
		"alice@example.com",
		"bob@example.com",
		"liqiang@gmail.com",
	} {
		if got, want := NamespaceForEmail(email), oracle(email); got != want {
			t.Errorf("NamespaceForEmail(%q) = %q, oracle = %q", email, got, want)
		}
	}
}

func TestOwnerEmailFromAccessToken_ExtractsEmailClaim(t *testing.T) {
	tok := makeAccessJWT(t, map[string]any{
		"email":    "alice@example.com",
		"token_id": "abc-123",
		"exp":      time.Now().Add(time.Hour).Unix(),
	})
	got, err := OwnerEmailFromAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", got)
	}
}

func TestOwnerEmailFromAccessToken_Errors(t *testing.T) {
	cases := []struct {
		name, tok string
	}{
		{"not a jwt", "opaque-token"},
		{"missing email", makeAccessJWT(t, map[string]any{"token_id": "x"})},
		{"empty email", makeAccessJWT(t, map[string]any{"email": ""})},
		{"empty token", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OwnerEmailFromAccessToken(tc.tok); err == nil {
				t.Errorf("expected error for %q", tc.name)
			}
		})
	}
}

// makeAccessJWT mints an unsigned JWT with the given claims — same
// pattern as bootstrap_test.go's makeJWT but kept self-contained so
// the access tests don't depend on bootstrap_test's helper visibility.
func makeAccessJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := base64.RawURLEncoding.EncodeToString([]byte("not-real-sig"))
	return header + "." + payload + "." + sig
}
