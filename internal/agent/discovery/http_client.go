// HTTP client for the discovery surface. Used by `outpost discover
// probe <url>` and by the daemon's NAT-hint poller (Wave 3A.2) to
// turn a hint URL into a verified peer record.
//
// The probe flow is intentionally one round-trip per call: each of
// Hello/Probe/Peers is a separate HTTP request. Wave 3B may collapse
// these for latency if it matters; Wave 3A.1 prefers obvious
// stateless wire shape.
package discovery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client speaks the discovery HTTP surface against a remote outpost.
// Reuse one Client across calls; it pools the underlying *http.Client.
type Client struct {
	hc *http.Client
}

// NewClient returns a Client with sane defaults (10s timeout, follow
// redirects = no, since /discover responses should never redirect).
func NewClient() *Client {
	return &Client{
		hc: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ProbeResult is the structured outcome of a one-shot probe(url) call:
// we get the peer's hello info plus mutual signature verification when
// possible.
type ProbeResult struct {
	// Peer is the parsed Peer record extracted from the remote's
	// /hello response. Trust starts at Unverified and is promoted
	// to TOFU on successful mutual signature verification.
	Peer Peer

	// ServerVerified is true when the server's /probe response
	// carried a signature over our nonce AND the signature matched
	// the server's claimed pubkey AND the pubkey fingerprints to
	// the same PeerID returned in /hello.
	ServerVerified bool

	// SessionID is the discovery session established with the
	// remote; usable for follow-up /peers calls (caller retains
	// the Client to make them on the verified session).
	SessionID string
}

// Probe performs the full hello → probe round-trip against the
// remote URL. baseURL is the URL of the discovery HTTP listener, e.g.
// `http://192.168.1.42:17778`. The /api/v1/discover prefix is
// appended automatically.
//
// `selfSigner` is the local outpost's ed25519 host signer; we sign
// the server's challenge with it.
//
// `selfHello` is what we tell the remote about ourselves.
func (c *Client) Probe(ctx context.Context, baseURL string, selfSigner ssh.Signer, selfHello PeerHello) (*ProbeResult, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, errors.New("baseURL is empty")
	}
	if selfSigner == nil {
		return nil, errors.New("selfSigner is nil")
	}
	if !selfHello.PeerID.IsValid() {
		return nil, fmt.Errorf("selfHello.peer_id %q is not a valid SHA256 fingerprint", selfHello.PeerID)
	}

	// /hello — Tier 1
	helloResp, err := c.doHello(ctx, baseURL, selfHello)
	if err != nil {
		return nil, err
	}

	peer := helloResp.My.toPeer()
	peer.LastSeenAt = time.Now()
	peer.Sources = []Source{SourceHTTPProbe}

	out := &ProbeResult{
		Peer:      peer,
		SessionID: helloResp.SessionID,
	}

	// /probe — Tier 2 (signed-nonce mutual auth)
	if helloResp.Challenge == "" {
		// Server didn't issue a challenge — Tier-1-only mode.
		// We still return the Peer with TrustUnverified.
		return out, nil
	}
	challenge, err := base64.StdEncoding.DecodeString(helloResp.Challenge)
	if err != nil {
		return out, fmt.Errorf("decode server challenge: %w", err)
	}

	// Sign the server's nonce with our ed25519 host key.
	signed, err := selfSigner.Sign(rand.Reader, challenge)
	if err != nil {
		return out, fmt.Errorf("sign challenge: %w", err)
	}
	selfPubkey := edPubkeyFromSigner(selfSigner)
	if selfPubkey == nil {
		return out, errors.New("self host key is not ed25519 — cannot sign discovery probes")
	}

	// Issue our own challenge for the server to sign back.
	ourNonce := make([]byte, 32)
	if _, err := rand.Read(ourNonce); err != nil {
		return out, fmt.Errorf("gen our nonce: %w", err)
	}

	probeReq := ProbeRequest{
		SessionID:        helloResp.SessionID,
		SignedChallenge:  base64.StdEncoding.EncodeToString(signed.Blob),
		YourChallenge:    base64.StdEncoding.EncodeToString(ourNonce),
		YourCallerPubkey: base64.StdEncoding.EncodeToString(selfPubkey),
	}
	probeResp, err := c.doProbe(ctx, baseURL, probeReq)
	if err != nil {
		return out, err
	}
	if !probeResp.OK {
		return out, errors.New("server returned probe ok=false")
	}

	// Verify the server's signature over our nonce — closes the
	// mutual-auth loop. Promote trust to TOFU.
	if probeResp.SignedYourChallenge != "" && probeResp.ServerPubkey != "" {
		sig, derr := base64.StdEncoding.DecodeString(probeResp.SignedYourChallenge)
		pk, perr := base64.StdEncoding.DecodeString(probeResp.ServerPubkey)
		if derr == nil && perr == nil && len(sig) == ed25519.SignatureSize && len(pk) == ed25519.PublicKeySize {
			if ed25519.Verify(ed25519.PublicKey(pk), ourNonce, sig) {
				// Pubkey must also fingerprint to the PeerID the
				// server claimed in /hello — guards against
				// server lying about its identity.
				sshPub, err := ssh.NewPublicKey(ed25519.PublicKey(pk))
				if err == nil && PeerID(ssh.FingerprintSHA256(sshPub)) == helloResp.My.PeerID {
					out.ServerVerified = true
					out.Peer.Trust = TrustTOFU
				}
			}
		}
	}

	return out, nil
}

// FetchPeers calls GET /api/v1/discover/peers on a verified session.
// Returns an error when the session isn't verified (server replies 403)
// or expired.
func (c *Client) FetchPeers(ctx context.Context, baseURL, sessionID string) ([]Peer, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/v1/discover/peers?session_id=" + sessionID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get peers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get peers HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out PeersResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode peers: %w", err)
	}
	return out.Peers, nil
}

func (c *Client) doHello(ctx context.Context, baseURL string, self PeerHello) (*HelloResponse, error) {
	body, _ := json.Marshal(HelloRequest{My: self})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/api/v1/discover/hello", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post hello: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("hello HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var out HelloResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode hello: %w", err)
	}
	return &out, nil
}

func (c *Client) doProbe(ctx context.Context, baseURL string, in ProbeRequest) (*ProbeResponse, error) {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/api/v1/discover/probe", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("probe HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var out ProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode probe: %w", err)
	}
	return &out, nil
}

// toPeer extracts a Peer-shape from a PeerHello. The reverse path
// (Peer → PeerHello) isn't symmetric: Hello strips cache-side fields.
func (h PeerHello) toPeer() Peer {
	return Peer{
		ID:               h.PeerID,
		AgentName:        h.AgentName,
		AssignedHostname: h.AssignedHostname,
		OAuth2Email:      h.OAuth2Email,
		OSUsername:       h.OSUsername,
		Endpoints:        h.Endpoints,
		Version:          h.Version,
		CloudboxBase:     h.CloudboxBase,
		Paired:           h.Paired,
		Trust:            TrustUnverified,
	}
}
