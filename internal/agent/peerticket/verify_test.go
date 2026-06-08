package peerticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKeys generates a fresh ed25519 keypair + the matching PEM so
// the LoadPubkey path is exercised too.
func testKeys(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return priv, pub, pemStr
}

// mintTicket builds a peer-ticket JWT with the given claim overrides.
// Defaults match a happy-path 60s ticket scoped to "ssh".
type ticketOpts struct {
	Aud   []string
	Sub   string
	Role  string
	Scope []string
	Exp   time.Time
	Nbf   time.Time
	Iat   time.Time
	JTI   string
	Iss   string
}

func mintTicket(t *testing.T, priv ed25519.PrivateKey, o ticketOpts) string {
	t.Helper()
	now := time.Now()
	if o.Exp.IsZero() {
		o.Exp = now.Add(60 * time.Second)
	}
	if o.JTI == "" {
		o.JTI = "jti-" + t.Name()
	}
	if o.Iss == "" {
		o.Iss = "cloudbox"
	}
	if len(o.Aud) == 0 {
		o.Aud = []string{"outpost:peer-b"}
	}
	if o.Sub == "" {
		o.Sub = "alice"
	}
	if o.Role == "" {
		o.Role = "user"
	}
	if o.Scope == nil {
		o.Scope = []string{"ssh"}
	}
	claims := jwt.MapClaims{
		"iss":   o.Iss,
		"aud":   o.Aud,
		"sub":   o.Sub,
		"jti":   o.JTI,
		"exp":   o.Exp.Unix(),
		"iat":   now.Unix(),
		"role":  o.Role,
		"scope": o.Scope,
	}
	if !o.Nbf.IsZero() {
		claims["nbf"] = o.Nbf.Unix()
	}
	if !o.Iat.IsZero() {
		claims["iat"] = o.Iat.Unix()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestLoadPubkey_Empty(t *testing.T) {
	k, err := LoadPubkey("")
	if err != nil || k != nil {
		t.Fatalf("empty PEM: want (nil, nil), got (%v, %v)", k, err)
	}
}

func TestLoadPubkey_Malformed(t *testing.T) {
	if _, err := LoadPubkey("not a pem"); err == nil {
		t.Fatal("expected error for non-PEM input")
	}
}

func TestLoadPubkey_RoundTrip(t *testing.T) {
	_, pub, pemStr := testKeys(t)
	got, err := LoadPubkey(pemStr)
	if err != nil {
		t.Fatalf("LoadPubkey: %v", err)
	}
	if !ed25519.PublicKey(pub).Equal(got) {
		t.Fatal("LoadPubkey round-trip mismatch")
	}
}

func TestVerify_HappyPath(t *testing.T) {
	priv, pub, _ := testKeys(t)
	tok := mintTicket(t, priv, ticketOpts{})
	v := NewVerifier(0)
	claims, err := v.Verify(tok, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "alice" || claims.Role != "user" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestVerify_Expired(t *testing.T) {
	priv, pub, _ := testKeys(t)
	// Mint already-expired ticket — far enough in the past to clear
	// the 30s leeway window.
	tok := mintTicket(t, priv, ticketOpts{Exp: time.Now().Add(-2 * time.Minute)})
	v := NewVerifier(0)
	_, err := v.Verify(tok, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestVerify_ClockSkewLeeway(t *testing.T) {
	priv, pub, _ := testKeys(t)
	// Expired 10s ago — well inside the 30s default leeway.
	tok := mintTicket(t, priv, ticketOpts{Exp: time.Now().Add(-10 * time.Second)})
	v := NewVerifier(0)
	if _, err := v.Verify(tok, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	}); err != nil {
		t.Fatalf("want leeway to absorb 10s drift, got %v", err)
	}
}

func TestVerify_NotYetValid(t *testing.T) {
	priv, pub, _ := testKeys(t)
	future := time.Now().Add(2 * time.Minute)
	tok := mintTicket(t, priv, ticketOpts{Nbf: future, Exp: future.Add(60 * time.Second)})
	v := NewVerifier(0)
	_, err := v.Verify(tok, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("want ErrNotYetValid, got %v", err)
	}
}

func TestVerify_WrongAudience(t *testing.T) {
	priv, pub, _ := testKeys(t)
	tok := mintTicket(t, priv, ticketOpts{Aud: []string{"outpost:peer-a"}})
	v := NewVerifier(0)
	_, err := v.Verify(tok, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b", // receiver is peer-b; ticket was for peer-a
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("want ErrWrongAudience, got %v", err)
	}
}

func TestVerify_ScopeMissing(t *testing.T) {
	priv, pub, _ := testKeys(t)
	tok := mintTicket(t, priv, ticketOpts{Scope: []string{"sftp"}})
	v := NewVerifier(0)
	_, err := v.Verify(tok, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh", // ticket is sftp-only
	})
	if !errors.Is(err, ErrScopeMissing) {
		t.Fatalf("want ErrScopeMissing, got %v", err)
	}
}

func TestVerify_ScopeEmpty(t *testing.T) {
	priv, pub, _ := testKeys(t)
	tok := mintTicket(t, priv, ticketOpts{Scope: []string{}})
	v := NewVerifier(0)
	_, err := v.Verify(tok, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrScopeMissing) {
		t.Fatalf("want ErrScopeMissing for empty scope, got %v", err)
	}
}

func TestVerify_BadSignature(t *testing.T) {
	priv, _, _ := testKeys(t)
	_, wrongPub, _ := testKeys(t) // different pubkey
	tok := mintTicket(t, priv, ticketOpts{})
	v := NewVerifier(0)
	_, err := v.Verify(tok, VerifyOptions{
		Pubkey:           wrongPub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestVerify_Replayed(t *testing.T) {
	priv, pub, _ := testKeys(t)
	tok := mintTicket(t, priv, ticketOpts{JTI: "single-use-jti"})
	v := NewVerifier(0)
	opts := VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	}
	if _, err := v.Verify(tok, opts); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := v.Verify(tok, opts); !errors.Is(err, ErrReplayed) {
		t.Fatalf("want ErrReplayed on second verify, got %v", err)
	}
}

func TestVerify_NoPubkey(t *testing.T) {
	priv, _, _ := testKeys(t)
	tok := mintTicket(t, priv, ticketOpts{})
	v := NewVerifier(0)
	_, err := v.Verify(tok, VerifyOptions{
		Pubkey:           nil,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrNoPubkey) {
		t.Fatalf("want ErrNoPubkey, got %v", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	_, pub, _ := testKeys(t)
	v := NewVerifier(0)
	_, err := v.Verify("not.a.valid.jwt", VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("want ErrMalformed, got %v", err)
	}
}

func TestVerify_MissingJTI(t *testing.T) {
	priv, pub, _ := testKeys(t)
	// jwt.MapClaims will simply skip the field if empty — explicitly
	// build a token with no jti to exercise the "missing jti" branch.
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   "cloudbox",
		"aud":   []string{"outpost:peer-b"},
		"sub":   "alice",
		"exp":   now.Add(60 * time.Second).Unix(),
		"iat":   now.Unix(),
		"role":  "user",
		"scope": []string{"ssh"},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	v := NewVerifier(0)
	_, err = v.Verify(signed, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("want ErrMalformed for missing jti, got %v", err)
	}
}

func TestVerify_WrongSigningMethod(t *testing.T) {
	_, pub, _ := testKeys(t)
	// Build a token signed with HS256 (the lib's default for symmetric
	// keys) so we exercise the WithValidMethods gate. EdDSA is the
	// only method we accept — anything else is treated as malformed.
	claims := jwt.MapClaims{
		"iss":   "cloudbox",
		"aud":   []string{"outpost:peer-b"},
		"sub":   "alice",
		"exp":   time.Now().Add(60 * time.Second).Unix(),
		"jti":   "jti-wrong-method",
		"role":  "user",
		"scope": []string{"ssh"},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("secret-key"))
	if err != nil {
		t.Fatalf("sign hs256: %v", err)
	}
	v := NewVerifier(0)
	_, err = v.Verify(signed, VerifyOptions{
		Pubkey:           pub,
		ExpectedAudience: "outpost:peer-b",
		RequiredScope:    "ssh",
	})
	if err == nil {
		t.Fatal("expected error for wrong signing method")
	}
}
