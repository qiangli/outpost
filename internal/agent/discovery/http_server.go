// HTTP `/api/v1/discover/*` surface for peer-to-peer discovery.
//
// Tier-1 endpoints (open, no cert required):
//
//	POST /api/v1/discover/hello   — exchange Tier-1 metadata
//
// Tier-2 endpoints (cert-or-signed-nonce verified, session-keyed):
//
//	POST /api/v1/discover/probe   — caller signs server's nonce;
//	                                 verifies cert presence + match
//	GET  /api/v1/discover/peers   — list known peers (verified only)
//
// Wave 3A.2 will add /gossip and the full cloudbox-CA cert path.
// Wave 3A.1 keeps the surface ready for both: the Cert blob is
// already plumbed in PeerHello and the probe path verifies signatures
// without yet requiring the cert to be cloudbox-signed.
package discovery

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// PeerHello is the wire shape every /hello and /probe exchanges. The
// Cert field is optional in Wave 3A.1 (we don't issue them yet) and
// strictly required for Tier-2 ops in Wave 3A.2.
type PeerHello struct {
	PeerID           PeerID     `json:"peer_id"`
	AgentName        string     `json:"agent_name"`
	AssignedHostname string     `json:"assigned_hostname,omitempty"`
	OAuth2Email      string     `json:"oauth2_email,omitempty"`
	OSUsername       string     `json:"os_username,omitempty"`
	Endpoints        []Endpoint `json:"endpoints,omitempty"`
	Version          string     `json:"version,omitempty"`
	CloudboxBase     string     `json:"cloudbox_base,omitempty"`
	Paired           bool       `json:"paired"`
	// HostCert is the cloudbox-CA-signed `ssh.Certificate` blob
	// (ssh.MarshalAuthorizedKey output, base64'd). Empty in Wave
	// 3A.1; populated in Wave 3A.2 after the cloudbox CA endpoint
	// lands.
	HostCert string `json:"host_cert,omitempty"`
}

// HelloRequest is the body of POST /api/v1/discover/hello.
type HelloRequest struct {
	SessionID string    `json:"session_id,omitempty"`
	My        PeerHello `json:"my"`
}

// HelloResponse is the response shape.
type HelloResponse struct {
	SessionID string    `json:"session_id"`
	My        PeerHello `json:"my"`                  // server's own info
	You       PeerHello `json:"you"`                 // server echoes the caller's claim
	Challenge string    `json:"challenge,omitempty"` // base64 32-byte nonce
}

// ProbeRequest is the body of POST /api/v1/discover/probe.
type ProbeRequest struct {
	SessionID        string `json:"session_id"`
	SignedChallenge  string `json:"signed_challenge"`             // base64 ed25519 sig over server's nonce
	YourChallenge    string `json:"your_challenge,omitempty"`     // optional: a fresh nonce for the server to sign back
	YourCallerPubkey string `json:"your_caller_pubkey,omitempty"` // base64 ed25519 pubkey; needed for sig verification when no cert
}

// ProbeResponse is the response shape.
type ProbeResponse struct {
	SessionID           string `json:"session_id"`
	OK                  bool   `json:"ok"`
	SignedYourChallenge string `json:"signed_your_challenge,omitempty"` // server's sig over the caller's optional nonce
	ServerPubkey        string `json:"server_pubkey,omitempty"`         // base64 ed25519 pubkey so caller can verify
}

// PeersResponse is the body of GET /api/v1/discover/peers.
type PeersResponse struct {
	Peers []Peer `json:"peers"`
}

// Server hosts the HTTP discovery surface. Construction is dependency-
// injected: the daemon supplies the local PeerHello (what we advertise
// about ourselves) and a function that returns the current peer cache
// (for /peers). The HostSigner is the outpost's ed25519 host key,
// reused from internal/agent/hostkey.go — we sign challenges with it.
type Server struct {
	mu       sync.RWMutex
	self     PeerHello
	signer   ssh.Signer
	sessions *SessionStore

	// peersFn returns the current snapshot of known peers. Pluggable
	// so the daemon can wire it to its discovery cache without this
	// package depending on cache internals.
	peersFn func() []Peer
}

// ServerOptions wires up dependencies.
type ServerOptions struct {
	Self    PeerHello
	Signer  ssh.Signer
	PeersFn func() []Peer
}

// NewServer constructs a discovery HTTP server. Self is what the
// server returns in /hello.My; PeersFn provides /peers content.
// HostSigner signs probe challenges.
func NewServer(opts ServerOptions) *Server {
	if opts.PeersFn == nil {
		opts.PeersFn = func() []Peer { return nil }
	}
	return &Server{
		self:     opts.Self,
		signer:   opts.Signer,
		sessions: NewSessionStore(),
		peersFn:  opts.PeersFn,
	}
}

// Mount registers handlers under the given prefix (default
// `/api/v1/discover`) on the supplied ServeMux. Caller controls the
// listener.
func (s *Server) Mount(mux *http.ServeMux, prefix string) {
	if prefix == "" {
		prefix = "/api/v1/discover"
	}
	mux.HandleFunc("POST "+prefix+"/hello", s.handleHello)
	mux.HandleFunc("POST "+prefix+"/probe", s.handleProbe)
	mux.HandleFunc("GET "+prefix+"/peers", s.handlePeers)
}

// handleHello — Tier 1. Returns the server's own PeerHello plus a
// fresh signed-nonce challenge. The session-id is generated server-side;
// the caller's optional session_id in the request is ignored (avoids
// caller-controlled session-table abuse).
func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	var req HelloRequest
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.My.PeerID == "" || !req.My.PeerID.IsValid() {
		http.Error(w, "my.peer_id missing or invalid", http.StatusBadRequest)
		return
	}

	sess, err := s.sessions.New(req.My.PeerID)
	if err != nil {
		http.Error(w, "session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	self := s.self
	s.mu.RUnlock()

	resp := HelloResponse{
		SessionID: sess.ID,
		My:        self,
		You:       req.My,
		Challenge: base64.StdEncoding.EncodeToString(sess.Nonce),
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleProbe — Tier 2. Verifies the caller's signature over the
// server-issued nonce. When the caller also supplies its own nonce
// (your_challenge), the server signs it back so the caller can verify
// the server's identity in turn.
//
// Wave 3A.1: signature verification uses the caller's submitted
// pubkey (TOFU). Wave 3A.2 will require a cloudbox-CA-signed cert
// and verify against the cert's embedded pubkey.
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var req ProbeRequest
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sess, ok := s.sessions.Get(req.SessionID)
	if !ok {
		http.Error(w, "session not found or expired", http.StatusUnauthorized)
		return
	}

	// Verify the caller's signature over our nonce.
	pubkeyBytes, err := base64.StdEncoding.DecodeString(req.YourCallerPubkey)
	if err != nil || len(pubkeyBytes) != ed25519.PublicKeySize {
		http.Error(w, "your_caller_pubkey missing or wrong length", http.StatusBadRequest)
		return
	}
	sig, err := base64.StdEncoding.DecodeString(req.SignedChallenge)
	if err != nil || len(sig) != ed25519.SignatureSize {
		http.Error(w, "signed_challenge missing or wrong size", http.StatusBadRequest)
		return
	}
	if !ed25519.Verify(ed25519.PublicKey(pubkeyBytes), sess.Nonce, sig) {
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}
	// Confirm the pubkey matches the PeerID claimed in /hello —
	// otherwise an attacker could present an arbitrary pubkey + sig.
	if !pubkeyMatchesPeerID(pubkeyBytes, sess.PeerID) {
		http.Error(w, "pubkey does not match claimed peer_id", http.StatusUnauthorized)
		return
	}

	s.sessions.MarkVerified(req.SessionID)

	resp := ProbeResponse{
		SessionID: req.SessionID,
		OK:        true,
	}
	// Mutual verification: server signs caller's nonce if provided.
	if req.YourChallenge != "" && s.signer != nil {
		callerNonce, err := base64.StdEncoding.DecodeString(req.YourChallenge)
		if err == nil && len(callerNonce) > 0 {
			signed, err := s.signer.Sign(nil, callerNonce)
			if err == nil {
				resp.SignedYourChallenge = base64.StdEncoding.EncodeToString(signed.Blob)
				if pk := edPubkeyFromSigner(s.signer); pk != nil {
					resp.ServerPubkey = base64.StdEncoding.EncodeToString(pk)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePeers — Tier 2. Requires a verified session.
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		http.Error(w, "session not found or expired", http.StatusUnauthorized)
		return
	}
	if !sess.Verified {
		http.Error(w, "session not verified — call /probe first", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, PeersResponse{Peers: s.peersFn()})
}

// Self updates the local PeerHello returned by /hello.My. Called by
// the daemon when discovered metadata changes (e.g., pairing
// completes, cert renewed).
func (s *Server) Self() PeerHello {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.self
}

// SetSelf swaps the advertised PeerHello atomically.
func (s *Server) SetSelf(self PeerHello) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.self = self
}

// --- helpers ---

func decodeJSONBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// pubkeyMatchesPeerID checks that an ed25519 pubkey, when wrapped as
// an ssh.PublicKey, fingerprints to the claimed PeerID. This is the
// "you can't claim to be peer X while presenting peer Y's pubkey"
// guard.
func pubkeyMatchesPeerID(pubkey []byte, claimed PeerID) bool {
	sshPub, err := ssh.NewPublicKey(ed25519.PublicKey(pubkey))
	if err != nil {
		return false
	}
	return PeerID(ssh.FingerprintSHA256(sshPub)) == claimed
}

// edPubkeyFromSigner extracts the raw ed25519 pubkey bytes from an
// ssh.Signer whose underlying key is ed25519. Returns nil for other
// key types — those should never happen given how we generate the
// outpost host key (always ed25519) but we fail soft rather than
// panicking.
func edPubkeyFromSigner(s ssh.Signer) []byte {
	if s == nil {
		return nil
	}
	pub := s.PublicKey()
	cpk, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return nil
	}
	edPub, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil
	}
	return []byte(edPub)
}

// (Unused helper for now; will be needed by Wave 3A.2 when verifying
// the embedded cert pubkey. Kept here to anchor the import that the
// Wave 3A.2 work will rely on.)
var _ = func() []byte {
	var b []byte
	return bytes.Clone(b)
}
var _ time.Time
