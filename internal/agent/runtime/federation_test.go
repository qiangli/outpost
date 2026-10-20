package runtime

import (
	"context"
	"strings"
	"testing"
)

func federatedOptions(o Options) Options {
	o.FederationHost = "192.0.2.10"
	o.FederationPort = 7000
	o.FederationToken = "tunnel-token"
	o.FederationSecret = "stcp-secret"
	return o
}

func TestFederationArgs(t *testing.T) {
	if got := federationArgs(Options{FederationHost: "192.0.2.10"}); got != nil {
		t.Fatalf("host without secret rendered %q", got)
	}
	got := strings.Join(federationArgs(federatedOptions(Options{})), " ")
	for _, want := range []string{
		"-p 127.0.0.1:15443:15443",
		"-e OUTPOST_FEDERATION_HOST=192.0.2.10",
		"-e OUTPOST_FEDERATION_STCP_SECRET=stcp-secret",
		"-e OUTPOST_FEDERATION_BIND_PORT=15443",
		"-e OUTPOST_FEDERATION_SERVER_PORT=7000",
		"-e OUTPOST_FEDERATION_TOKEN=tunnel-token",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("federationArgs missing %q: %s", want, got)
		}
	}
}

func TestUpFederationPublishesVisitorOnHostLoopbackOnly(t *testing.T) {
	h := newRuntimeHarness(t)
	if err := Up(context.Background(), federatedOptions(h.options())); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	run := commandStarting(t, h.commands(), "run ")
	if !strings.Contains(run, "-p 127.0.0.1:15443:15443") || !strings.Contains(run, "OUTPOST_FEDERATION_HOST=192.0.2.10") {
		t.Fatalf("federated runtime missing visitor publish/env: %s", run)
	}
	for _, forbidden := range []string{"-p 0.0.0.0:15443", "-p 15443:", "-p :15443"} {
		if strings.Contains(run, forbidden) {
			t.Fatalf("federation visitor escaped host loopback (%q): %s", forbidden, run)
		}
	}
}

func TestUpUnfederatedRuntimeHasNoFederationArgs(t *testing.T) {
	h := newRuntimeHarness(t)
	if err := Up(context.Background(), h.options()); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if run := commandStarting(t, h.commands(), "run "); strings.Contains(run, "FEDERATION") || strings.Contains(run, "15443") {
		t.Fatalf("unfederated runtime carries federation args: %s", run)
	}
}

// Joining, leaving or a rotated secret must recreate the container; an
// unfederated runtime keeps its pre-federation fingerprint (no churn on
// upgrade).
func TestRuntimeFingerprintCoversFederation(t *testing.T) {
	base := Options{AgentName: "n", NodeToken: "t"}
	fp := func(o Options) string {
		s, err := runtimeFingerprint(t.Context(), "false", "img", o)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	plain := fp(base)
	joined := federatedOptions(base)
	if fp(joined) == plain {
		t.Error("joining did not change the fingerprint")
	}
	rotated := joined
	rotated.FederationSecret = "rotated"
	if fp(rotated) == fp(joined) {
		t.Error("rotating the federation secret did not change the fingerprint")
	}
	halfConfigured := base
	halfConfigured.FederationHost = "192.0.2.10"
	if fp(halfConfigured) != plain {
		t.Error("an inactive federation block changed the fingerprint")
	}
}
