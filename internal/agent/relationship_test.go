package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestHostGrantsRelationship(t *testing.T) {
	h := NewHostGrants("owner@example.com")
	h.Update("sat", &GrantCopy{
		Owner:   "owner@example.com",
		Members: []GrantMember{{Email: "rec@example.com", Circle: "recruiter"}},
		Groups:  map[string][]string{"hiring": {"REC@example.com"}, "chess": {"x@example.com"}},
	})
	if RelAccount != "account" {
		t.Fatalf("RelAccount = %q, want %q", RelAccount, "account")
	}
	for _, tc := range []struct {
		user   string
		kind   string
		circle string
		groups int
	}{
		{"", "", "", 0},
		{"Owner@Example.com", RelOwner, "", 0},
		{"rec@example.com", RelMember, "recruiter", 1},
		{"stranger@example.com", RelAccount, "", 0},
	} {
		got := h.Relationship(tc.user)
		if got.Kind != tc.kind || got.Circle != tc.circle || len(got.Groups) != tc.groups {
			t.Errorf("Relationship(%q) = %+v, want kind=%q circle=%q groups=%d", tc.user, got, tc.kind, tc.circle, tc.groups)
		}
	}
	var nilStore *HostGrants
	if got := nilStore.Relationship("a@example.com"); got.Kind != RelAccount {
		t.Errorf("nil store must fail closed, got %+v", got)
	}
}

// A trust-identity app behind a host that knows its relationships gets the
// caller's relationship stamped and signed; a logged-in account holder who is
// not a member reaches it as a pseudonym only, never as an address.
func TestProxy_RelationshipStamp(t *testing.T) {
	type seen struct {
		user, email, rel, circle, groups, visitor, sig, ts, prefix string
	}
	gotCh := make(chan seen, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCh <- seen{
			user:    r.Header.Get("Remote-User") + r.Header.Get("X-Periscope-User"),
			email:   r.Header.Get("Remote-Email"),
			rel:     r.Header.Get("X-Outpost-Relationship"),
			circle:  r.Header.Get("X-Outpost-Circle"),
			groups:  r.Header.Get("X-Outpost-Groups"),
			visitor: r.Header.Get("X-Outpost-Visitor"),
			sig:     r.Header.Get("X-Outpost-Relationship-Sig"),
			ts:      r.Header.Get("X-Outpost-Identity-Ts"),
			prefix:  r.Header.Get("X-Forwarded-Prefix"),
		}
	}))
	t.Cleanup(upstream.Close)
	uhost, uport := splitHostPort(t, upstream.URL)

	const secret = "s3cret"
	reg := NewAppRegistry()
	reg.trustLoopback = true
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "site", Scheme: "http", Host: uhost, Port: uport, Enabled: true,
		TrustCloudIdentity: true, SSOSecret: secret,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	grants := NewHostGrants("owner@example.com")
	grants.Update("sat", &GrantCopy{
		Owner:   "owner@example.com",
		Members: []GrantMember{{Email: "rec@example.com", Circle: "recruiter"}},
		Groups:  map[string][]string{"hiring": {"rec@example.com"}},
	})
	reg.SetHostGrants(grants)
	front := httptest.NewServer(newTestRouter(reg))
	t.Cleanup(front.Close)

	get := func(t *testing.T, user string, forged map[string]string) seen {
		t.Helper()
		req, _ := http.NewRequest("GET", front.URL+"/app/site/", nil)
		req.Header.Set("X-Forwarded-Prefix", "/matrix/h/host-a/app/site")
		if user != "" {
			req.Header.Set("X-Periscope-User", user)
		}
		for k, v := range forged {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_ = resp.Body.Close()
		select {
		case got := <-gotCh:
			return got
		case <-time.After(time.Second):
			t.Fatal("upstream never received the request")
			return seen{}
		}
	}
	verify := func(t *testing.T, got seen, user string) {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(relationshipPayload(got.rel, got.circle, got.groups, got.visitor, user, got.prefix, got.ts)))
		if got.sig != hex.EncodeToString(mac.Sum(nil)) {
			t.Errorf("relationship signature does not verify for %+v", got)
		}
	}

	t.Run("member carries circle and groups", func(t *testing.T) {
		got := get(t, "rec@example.com", nil)
		if got.rel != RelMember || got.circle != "recruiter" || got.groups != "hiring" || got.email != "rec@example.com" || got.visitor != "" {
			t.Errorf("member stamp = %+v", got)
		}
		verify(t, got, "rec@example.com")
	})
	t.Run("owner", func(t *testing.T) {
		got := get(t, "owner@example.com", nil)
		if got.rel != RelOwner || got.email != "owner@example.com" {
			t.Errorf("owner stamp = %+v", got)
		}
		verify(t, got, "owner@example.com")
	})
	t.Run("stranger gets a pseudonym, never the address", func(t *testing.T) {
		got := get(t, "stranger@example.com", nil)
		if got.rel != RelAccount || got.user != "" || got.email != "" || got.circle != "" {
			t.Errorf("stranger stamp leaked identity or wrong wire kind: %+v", got)
		}
		if got.visitor != visitorPseudonym(secret, "site", "stranger@example.com") || len(got.visitor) != 32 {
			t.Errorf("visitor = %q", got.visitor)
		}
		if got.visitor == visitorPseudonym(secret, "other", "stranger@example.com") {
			t.Error("pseudonym must differ per app")
		}
		verify(t, got, "")
	})
	t.Run("anonymous gets no relationship; forged copies are stripped", func(t *testing.T) {
		got := get(t, "", map[string]string{"X-Outpost-Relationship": "owner", "X-Outpost-Circle": "recruiter", "X-Outpost-Visitor": "x"})
		if got.rel != "" || got.circle != "" || got.visitor != "" || got.sig != "" {
			t.Errorf("anonymous stamp = %+v", got)
		}
	})
}
