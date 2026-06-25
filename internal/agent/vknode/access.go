package vknode

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// Access is the security gate vknode.CreatePod consults before
// scheduling a pod on this outpost. It holds the set of Kubernetes
// namespace names that are permitted to schedule workloads here —
// derived from the outpost's owner (always) and the outpost's
// share-receivers (in the multi-user model, once that wires up
// against cloudbox).
//
// The namespace naming convention is shared with cloudbox-side
// cluster.userNamespace: each cloudbox user has a namespace
// `user-<6-byte-sha256-of-email-hex>`. The outpost computes the
// same hash from the email it knows about, builds the allowed-name,
// and checks pod.Namespace against the set on every CreatePod.
//
// A nil *Access means "no check" — pods from any namespace are
// accepted. Used in dev/single-tenant scenarios where the operator
// wants to verify the embedded cluster works before turning on
// access enforcement. Once the cloudbox-side access endpoint exists,
// startClusterRunner will always construct a non-nil Access.
type Access struct {
	mu      sync.RWMutex
	allowed map[string]struct{}
}

// NewAccess returns an Access containing the given namespaces. Pass
// at least the outpost owner's namespace; in the future this will
// also include each share-receiver's namespace, refreshed from
// cloudbox via Refresher.
func NewAccess(namespaces ...string) *Access {
	a := &Access{allowed: make(map[string]struct{}, len(namespaces))}
	for _, n := range namespaces {
		n = strings.TrimSpace(n)
		if n != "" {
			a.allowed[n] = struct{}{}
		}
	}
	return a
}

// Allowed reports whether ns may schedule pods on this outpost. nil
// receiver returns true so unconfigured outposts still work — the
// security choice is made by whoever constructs (or doesn't
// construct) the Access.
func (a *Access) Allowed(ns string) bool {
	if a == nil {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.allowed[ns]
	return ok
}

// Set replaces the allowed-namespace set atomically. Used by the
// refresher when cloudbox's share data changes.
func (a *Access) Set(namespaces ...string) {
	if a == nil {
		return
	}
	next := make(map[string]struct{}, len(namespaces))
	for _, n := range namespaces {
		n = strings.TrimSpace(n)
		if n != "" {
			next[n] = struct{}{}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allowed = next
}

// Snapshot returns the current allowed-namespace set as a slice. Used
// by status / debug surfaces; not on the hot path.
func (a *Access) Snapshot() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.allowed))
	for n := range a.allowed {
		out = append(out, n)
	}
	return out
}

// NamespaceForEmail computes the per-user workload namespace name for
// an email — matches cloudbox/internal/cluster.userNamespace exactly.
// Format: "user-<12-hex-chars>" where the hex is the first 6 bytes
// of the SHA-256 of the lowercased+trimmed email.
func NamespaceForEmail(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "user-" + hex.EncodeToString(h[:6])
}

// OwnerEmailFromAccessToken decodes the email claim out of a cloudbox-
// issued access_token without verifying the signature. The token came
// from cloudbox over TLS and was already validated against
// JWTTokenSecret on the cloudbox side; here we just need to read the
// email payload so we can derive the owner's namespace.
//
// Returns the empty string + an error when the token isn't a parseable
// JWT or doesn't carry an email claim — callers treat that as "owner
// unknown" and either error out or fall back to nil-Access for the
// dev case.
func OwnerEmailFromAccessToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("vknode: access_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", err
		}
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.Email == "" {
		return "", errors.New("vknode: access_token has no email claim")
	}
	return claims.Email, nil
}
