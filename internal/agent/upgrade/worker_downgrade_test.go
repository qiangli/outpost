package upgrade

import (
	"context"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
)

// goodEnvelope returns a schema-valid envelope for the given release;
// the URL never gets fetched because the downgrade guard fires first.
func goodEnvelope(releaseID, commit string, force bool) Envelope {
	return Envelope{
		ReleaseID: releaseID,
		URL:       "https://example.com/outpost",
		SHA256:    "deadbeef",
		Commit:    commit,
		Force:     force,
	}
}

func withDevHost(h *harness) {
	h.setState(func(s *StateSnapshot) {
		s.CurrentVersion = "v1.0.0-rc.10-dev"
		s.CurrentVCSTime = "2026-10-20T00:00:00Z"
	})
}

// TestPushRefusesDowngrade: the cloudbox push path must NEVER move a host
// to a build older than what it runs. v0.32.0 over v1.0.0-rc.10-dev is
// the 2026-10-24 downgrade incident.
func TestPushRefusesDowngrade(t *testing.T) {
	h := newHarness(t)
	withDevHost(h)
	r := h.worker.Apply(context.Background(), goodEnvelope("v0.32.0", "aaaaaaa", false))
	if r.Status != StatusDowngrade {
		t.Fatalf("Apply older release = %q (%s), want %q", r.Status, r.Detail, StatusDowngrade)
	}
}

// TestPushRefusesDowngradeDespiteForce: --force / Force=true keeps its
// current meaning (same commit) and does NOT allow downgrades.
func TestPushRefusesDowngradeDespiteForce(t *testing.T) {
	h := newHarness(t)
	withDevHost(h)
	r := h.worker.Apply(context.Background(), goodEnvelope("v0.32.0", "aaaaaaa", true))
	if r.Status != StatusDowngrade {
		t.Fatalf("Apply older release with Force = %q (%s), want %q", r.Status, r.Detail, StatusDowngrade)
	}
}

// TestPushAcceptsNewer: a strictly newer release still flows to accepted.
func TestPushAcceptsNewer(t *testing.T) {
	h := newHarness(t)
	withDevHost(h)
	r := h.worker.Apply(context.Background(), goodEnvelope("v1.0.0-rc.12-dev", "bbbbbbb", false))
	if r.Status != StatusAccepted {
		t.Fatalf("Apply newer release = %q (%s), want accepted", r.Status, r.Detail)
	}
}

// TestPushSkipsGuardForOpaqueIDs: release IDs that are not semver carry
// no ordering, so the guard leaves them alone (back-compat).
func TestPushSkipsGuardForOpaqueIDs(t *testing.T) {
	h := newHarness(t)
	withDevHost(h)
	r := h.worker.Apply(context.Background(), goodEnvelope("rel-2026-10-24", "ccccccc", false))
	if r.Status != StatusAccepted {
		t.Fatalf("Apply opaque release = %q (%s), want accepted", r.Status, r.Detail)
	}
}

// TestShouldDeclineCandidate is the shared CLI/push guard: refuse when the
// candidate is provably older, allow when either side has no semver to
// order by (probe + sha checks still apply downstream).
func TestShouldDeclineCandidate(t *testing.T) {
	cur := agent.BuildInfo{Version: "v1.0.0-rc.10-dev", VCSTime: "2026-10-20T00:00:00Z"}
	cases := []struct {
		name      string
		cur       agent.BuildInfo
		releaseID string
		buildTime string
		want      bool
	}{
		{"older stable refused", cur, "v0.32.0", "", true},
		{"older stable refused even with newer timestamp", cur, "v0.32.0", "2026-10-23T00:00:00Z", true},
		{"newer rc accepted", cur, "v1.0.0-rc.12-dev", "2026-10-23T00:00:00Z", false},
		{"same version refused (lateral, not strictly newer)", cur, "v1.0.0-rc.10-dev", "", true},
		{"unparseable candidate allowed", cur, "not-a-version", "", false},
		{"unparseable current allowed", agent.BuildInfo{Version: ""}, "v0.32.0", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ShouldDeclineCandidate(tc.cur, tc.releaseID, tc.buildTime)
			if got != tc.want {
				t.Errorf("ShouldDeclineCandidate(%q, %q) = %v, want %v (reason %q)",
					tc.cur.Version, tc.releaseID, got, tc.want, reason)
			}
			if got && reason == "" {
				t.Error("decline without a reason — the refusal message must name both versions")
			}
		})
	}
}
