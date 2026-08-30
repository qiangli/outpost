package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// THE REGRESSION THIS FILE EXISTS FOR.
//
// portal.Reattach has always decoded `cloudbox_ticket_pubkey`, and its doc
// comment promises the field is re-published on every reattach "so a re-pair
// after a cloudbox key rotation picks up the new key without an explicit
// migration step". tryReattach then merged Token, RemotePort, ServerAddr,
// ServerPort, Protocol and Cluster — and silently dropped the pubkey.
//
// So the value could only ever be written by the FIRST-PAIRING path. Every
// host paired before cloudbox had a ticket signer kept an empty pubkey
// forever. sshHandler gates the entire peer-ticket branch on
// `len(deps.TicketPubkey) > 0`, so an empty key means a legitimate ticket is
// never even examined: cloudboxVouched stays false, NoClientAuth is never
// set, and the caller is rejected with `ssh: unable to authenticate,
// attempted methods [none]` — an error that names the CLIENT for a fault that
// lives entirely in the RECEIVER's config.
//
// Observed live: mesh-direct ssh resolved the peer, opened the forward, got a
// ticket issued and completed the WebSocket handshake, then failed at the SSH
// auth layer on every host in the fleet.

const testTicketPubkeyPEM = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAGb9ECWmEzf6FQbrBZ9w7lshQhqowtrbLDFw4rXAxZuE=\n-----END PUBLIC KEY-----\n"

// reattachStub serves /api/register/reattach with the given response body and
// returns a FileConfig already pointed at it.
func reattachStub(t *testing.T, body map[string]any) (*conf.FileConfig, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/register/reattach" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}
	fc := &conf.FileConfig{
		AgentName:   "test-host",
		AccessToken: "test-access-token",
		ServerAddr:  u.Hostname(),
		ServerPort:  port,
		Protocol:    "ws", // cloudboxHTTPBase -> http
	}
	return fc, srv.Close
}

// The empty-to-populated case: the exact state every pre-signer host is in.
func TestTryReattach_AdoptsTicketPubkeyWhenAbsent(t *testing.T) {
	fc, stop := reattachStub(t, map[string]any{
		"agent_name":             "test-host",
		"cloudbox_ticket_pubkey": testTicketPubkeyPEM,
	})
	defer stop()

	if fc.CloudboxTicketPubkey != "" {
		t.Fatal("precondition: pubkey must start empty")
	}
	got, err := tryReattach(context.Background(), fc, "")
	if err != nil {
		t.Fatalf("tryReattach: %v", err)
	}
	if got.CloudboxTicketPubkey != testTicketPubkeyPEM {
		t.Errorf("pubkey = %q, want it adopted from the reattach response", got.CloudboxTicketPubkey)
	}
}

// Rotation: a NEW key must REPLACE an existing one. Merge-if-absent would
// pin every host to the first key it ever saw and break key rotation.
func TestTryReattach_ReplacesTicketPubkeyOnRotation(t *testing.T) {
	fc, stop := reattachStub(t, map[string]any{
		"agent_name":             "test-host",
		"cloudbox_ticket_pubkey": testTicketPubkeyPEM,
	})
	defer stop()
	fc.CloudboxTicketPubkey = "-----BEGIN PUBLIC KEY-----\nSTALE\n-----END PUBLIC KEY-----\n"

	got, err := tryReattach(context.Background(), fc, "")
	if err != nil {
		t.Fatalf("tryReattach: %v", err)
	}
	if got.CloudboxTicketPubkey != testTicketPubkeyPEM {
		t.Errorf("pubkey = %q, want the rotated key", got.CloudboxTicketPubkey)
	}
}

// A cloudbox with no ticket signer omits the field. That must NOT erase a
// working key — losing it silently disables passwordless peer-direct ssh, the
// same outage this file documents, just in the other direction.
func TestTryReattach_EmptyResponseDoesNotEraseTicketPubkey(t *testing.T) {
	fc, stop := reattachStub(t, map[string]any{
		"agent_name": "test-host",
	})
	defer stop()
	fc.CloudboxTicketPubkey = testTicketPubkeyPEM

	got, err := tryReattach(context.Background(), fc, "")
	if err != nil {
		t.Fatalf("tryReattach: %v", err)
	}
	if got.CloudboxTicketPubkey != testTicketPubkeyPEM {
		t.Errorf("pubkey = %q, want the existing key preserved", got.CloudboxTicketPubkey)
	}
}
