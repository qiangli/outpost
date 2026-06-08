// Package peerticket verifies short-lived JWTs ("peer tickets")
// cloudbox issues at `POST /api/v1/ssh/peer-ticket`. The local outpost
// trades its matrix_elev cookie for one of these tickets, then
// presents it to a peer outpost on the LAN-direct path. The receiving
// outpost calls Verify with its stored CloudboxTicketPubkey and, on
// success, treats the connection as cloudbox-vouched without the
// X-Periscope-Role header that cloudbox stamps on the matrix-tunnel
// proxied path.
//
// The cookie itself never leaves the original client ↔ cloudbox
// channel — only the derived ticket traverses the LAN.
//
// Replay defense is a process-memory LRU keyed by `jti`. 60s TTLs
// keep the replay window naturally bounded; restart-blast-radius is
// smaller than the TTL window so persisting jti would be pointless.
package peerticket

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the verified peer-ticket payload, projected onto the
// fields the outpost actually uses. Anything cloudbox adds in the
// future that we don't read here is silently ignored — fine for
// additive evolution.
type Claims struct {
	Issuer    string    // "cloudbox" — informational; not gated on
	Audience  string    // "outpost:<peer agent_name>" — must equal receiver
	Subject   string    // OS user the peer should impersonate
	Role      string    // "user" or "admin" — feeds the X-Periscope-Role-equivalent decision
	Scope     []string  // capability strings the ticket grants ("ssh", "sftp", "backup", …)
	ExpiresAt time.Time // hard cutoff (cloudbox issues 60s tickets)
	NotBefore time.Time // optional; honored if set
	IssuedAt  time.Time // informational; not gated on
	JTI       string    // unique-per-issuance; replay-protected by Verifier
}

// rawClaims is the wire shape. jwt/v5's parsing populates this; we
// project into Claims after the lib's standard checks pass.
type rawClaims struct {
	jwt.RegisteredClaims
	Role  string   `json:"role"`
	Scope []string `json:"scope"`
}

// GetExpirationTime is on jwt.RegisteredClaims; we override nothing.
// jwt/v5 calls these methods to drive its validation; embedding
// RegisteredClaims is enough.

// VerifyOptions threads the receiver-side invariants into Verify.
// ExpectedAudience and RequiredScope are mandatory — fail-closed on
// empty values so a misconfigured caller can't accidentally widen
// trust.
type VerifyOptions struct {
	// Pubkey is the cloudbox ed25519 verification key, loaded from
	// FileConfig.CloudboxTicketPubkey via LoadPubkey.
	Pubkey ed25519.PublicKey

	// ExpectedAudience must match the ticket's `aud` claim exactly.
	// The receiver passes its own identity ("outpost:" + AgentName)
	// — never a value taken from the request. Prevents an attacker
	// who captured a ticket scoped to peer-A from replaying it to
	// peer-B.
	ExpectedAudience string

	// RequiredScope is the capability the receiver is gating right
	// now ("ssh" for the WS-SSH handler, "sftp" or "backup" for
	// future per-route gates). The ticket's `scope` claim MUST
	// contain this value.
	RequiredScope string

	// ClockSkew widens both the `nbf` and `exp` windows by this
	// amount to absorb modest clock drift between cloudbox and the
	// receiver. Default 30s when zero.
	ClockSkew time.Duration

	// Now overrides time.Now() for tests. Zero-valued = real clock.
	Now time.Time
}

// Sentinel errors so callers (sshHandler, future capability handlers)
// can distinguish "this ticket is malformed" from "this ticket is
// expired" from "this ticket was already consumed."
var (
	ErrMalformed     = errors.New("peerticket: malformed")
	ErrBadSignature  = errors.New("peerticket: signature invalid")
	ErrExpired       = errors.New("peerticket: expired")
	ErrNotYetValid   = errors.New("peerticket: not yet valid")
	ErrWrongAudience = errors.New("peerticket: wrong audience")
	ErrScopeMissing  = errors.New("peerticket: required scope missing")
	ErrReplayed      = errors.New("peerticket: jti already consumed")
	ErrNoPubkey      = errors.New("peerticket: no verification key configured")
)

// Verifier holds receiver-side state — currently just the replay-
// protection LRU. One Verifier per process; safe for concurrent use.
type Verifier struct {
	mu   sync.Mutex
	jtis map[string]time.Time // jti → expiration; pruned opportunistically
	cap  int                  // soft cap; oldest entries evicted when exceeded
}

// DefaultJTICap is the LRU cap. 4096 entries × ~60s TTL bounds memory
// at ~256KiB worst case — trivial. Lower this if the receiver runs in
// a heavily memory-constrained environment.
const DefaultJTICap = 4096

// NewVerifier returns a Verifier ready to use. cap ≤ 0 means use
// DefaultJTICap.
func NewVerifier(cap int) *Verifier {
	if cap <= 0 {
		cap = DefaultJTICap
	}
	return &Verifier{
		jtis: make(map[string]time.Time, cap),
		cap:  cap,
	}
}

// Verify parses the JWT, verifies its signature against Pubkey,
// validates every claim against opts, and consumes the jti against
// the replay LRU. Returns the projected Claims on success.
func (v *Verifier) Verify(token string, opts VerifyOptions) (*Claims, error) {
	if len(opts.Pubkey) == 0 {
		return nil, ErrNoPubkey
	}
	if strings.TrimSpace(opts.ExpectedAudience) == "" {
		return nil, fmt.Errorf("peerticket: ExpectedAudience required")
	}
	if strings.TrimSpace(opts.RequiredScope) == "" {
		return nil, fmt.Errorf("peerticket: RequiredScope required")
	}
	skew := opts.ClockSkew
	if skew <= 0 {
		skew = 30 * time.Second
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	// Parser is built per-call so we can pin the skew and the
	// expected method. EdDSA is the only signature method cloudbox
	// emits today; anything else is treated as a protocol violation.
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithLeeway(skew),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)

	var raw rawClaims
	parsed, err := parser.ParseWithClaims(token, &raw, func(t *jwt.Token) (any, error) {
		return opts.Pubkey, nil
	})
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, ErrExpired
		case errors.Is(err, jwt.ErrTokenNotValidYet):
			return nil, ErrNotYetValid
		case errors.Is(err, jwt.ErrTokenSignatureInvalid),
			errors.Is(err, jwt.ErrSignatureInvalid):
			return nil, ErrBadSignature
		}
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if parsed == nil || !parsed.Valid {
		return nil, ErrMalformed
	}

	// Audience: jwt/v5 stores `aud` as a slice; we accept the ticket
	// if any entry equals ExpectedAudience exactly. Comparing against
	// the receiver's own identity (never a request-supplied value)
	// is what prevents ticket-for-peerA-replayed-to-peerB.
	if !slices.Contains(raw.Audience, opts.ExpectedAudience) {
		return nil, ErrWrongAudience
	}

	// Scope: fail-closed if missing. An empty Scope claim doesn't
	// imply "grants everything" — it means "grants nothing."
	if !slices.Contains(raw.Scope, opts.RequiredScope) {
		return nil, ErrScopeMissing
	}

	// JTI replay-protection. Empty jti is treated as ErrMalformed —
	// cloudbox mints one per issuance, so an empty one is a
	// protocol violation. Consume-and-remember; concurrent verifies
	// on the same jti race on the mutex and exactly one wins.
	jti := strings.TrimSpace(raw.ID)
	if jti == "" {
		return nil, fmt.Errorf("%w: missing jti", ErrMalformed)
	}
	exp := time.Time{}
	if raw.ExpiresAt != nil {
		exp = raw.ExpiresAt.Time
	}
	if err := v.consume(jti, exp, now); err != nil {
		return nil, err
	}

	out := &Claims{
		Issuer:   raw.Issuer,
		Audience: opts.ExpectedAudience,
		Subject:  raw.Subject,
		Role:     raw.Role,
		Scope:    append([]string(nil), raw.Scope...),
		JTI:      jti,
	}
	if raw.ExpiresAt != nil {
		out.ExpiresAt = raw.ExpiresAt.Time
	}
	if raw.NotBefore != nil {
		out.NotBefore = raw.NotBefore.Time
	}
	if raw.IssuedAt != nil {
		out.IssuedAt = raw.IssuedAt.Time
	}
	return out, nil
}

// consume records jti as used. Returns ErrReplayed if it was already
// in the LRU. Opportunistically prunes expired entries on every call
// so the map stays bounded without a separate sweeper goroutine.
func (v *Verifier) consume(jti string, exp time.Time, now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, seen := v.jtis[jti]; seen {
		return ErrReplayed
	}
	// Prune expired entries inline. O(n) but n is bounded by cap;
	// cheaper than a heap and no goroutine to leak.
	if len(v.jtis) >= v.cap {
		for k, e := range v.jtis {
			if e.Before(now) {
				delete(v.jtis, k)
			}
		}
		// Still at cap after pruning? Drop arbitrary entries —
		// they're cryptographically valid jtis we've already
		// consumed, so further pruning won't open a replay window.
		// Hitting this branch under normal traffic means cap is
		// too low; bump DefaultJTICap.
		for k := range v.jtis {
			if len(v.jtis) < v.cap {
				break
			}
			delete(v.jtis, k)
		}
	}
	v.jtis[jti] = exp
	return nil
}

// LoadPubkey parses a PEM-encoded ed25519 public key (the format
// cloudbox publishes via /api/register/exchange). Empty input returns
// (nil, nil) so callers can distinguish "no key configured yet" from
// "configured but malformed."
func LoadPubkey(pemStr string) (ed25519.PublicKey, error) {
	s := strings.TrimSpace(pemStr)
	if s == "" {
		return nil, nil
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, fmt.Errorf("peerticket: pubkey PEM decode failed")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("peerticket: parse pubkey: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("peerticket: pubkey is not ed25519 (got %T)", key)
	}
	return pub, nil
}
