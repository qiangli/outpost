package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Same cases as the cloudbox-side rule evaluator.
func TestGrantCopyAdmits(t *testing.T) {
	base := GrantCopy{
		Owner: "o@x",
		Members: []GrantMember{
			{Email: "team@x", Circle: "team"},
			{Email: "teacher@x", Circle: "team"},
			{Email: "fam@x", Circle: "family"},
			{Email: "guest@x", Circle: "guest"},
		},
		Groups: map[string][]string{"teachers": {"teacher@x", "guest@x", "o@x"}},
	}
	cases := []struct {
		name  string
		rules []GrantRule
		email string
		want  bool
	}{
		{"circle rule admits", []GrantRule{{Circles: []string{"team"}}}, "team@x", true},
		{"other circle refused", []GrantRule{{Circles: []string{"team"}}}, "fam@x", false},
		{"circle+group refuses circle member outside group", []GrantRule{{Circles: []string{"team"}, Groups: []string{"teachers"}}}, "team@x", false},
		{"circle+group admits circle member in group", []GrantRule{{Circles: []string{"team"}, Groups: []string{"teachers"}}}, "teacher@x", true},
		{"circle+group refuses group member outside circles", []GrantRule{{Circles: []string{"team"}, Groups: []string{"teachers"}}}, "guest@x", false},
		{"group-only admits any circle", []GrantRule{{Groups: []string{"teachers"}}}, "guest@x", true},
		{"email narrows within circles", []GrantRule{{Circles: []string{"team"}, Emails: []string{"team@x"}}}, "team@x", true},
		{"email narrows within circles refuses other", []GrantRule{{Circles: []string{"team"}, Emails: []string{"team@x"}}}, "teacher@x", false},
		{"email outside circles refused", []GrantRule{{Circles: []string{"team"}, Emails: []string{"fam@x"}}}, "fam@x", false},
		{"multiple rules OR", []GrantRule{{Circles: []string{"team"}}, {Circles: []string{"family"}}}, "fam@x", true},
		{"empty list owner only", nil, "team@x", false},
		{"empty rule admits any member", []GrantRule{{}}, "guest@x", true},
		{"non-member refused", []GrantRule{{}}, "stranger@x", false},
		{"non-member named by email refused", []GrantRule{{Emails: []string{"stranger@x"}}}, "stranger@x", false},
		{"owner passes", nil, "o@x", true},
		{"case-insensitive caller", []GrantRule{{Circles: []string{"team"}}}, "TEAM@X", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.Apps = map[string][]GrantRule{"app": tc.rules}
			if got := c.Admits("app", tc.email); got != tc.want {
				t.Fatalf("Admits(%q) = %v, want %v", tc.email, got, tc.want)
			}
		})
	}
}

func TestHostGrantsNoCopyOwnerOnly(t *testing.T) {
	h := NewHostGrants("Owner@X")
	if !h.Allow("app", "owner@x") {
		t.Fatal("owner refused with no copy")
	}
	if h.Allow("app", "member@x") || h.Allow("app", "") {
		t.Fatal("non-owner admitted with no copy")
	}
	var nilStore *HostGrants
	if nilStore.Allow("app", "owner@x") {
		t.Fatal("nil store admitted")
	}
}

// grantSatellite serves a fixed copy at /api/v1/hosts/self/grants behind a
// bearer; fail makes it answer 500.
func grantSatellite(t *testing.T, token string, c *GrantCopy, fail *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/hosts/self/grants" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if fail != nil && *fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGrantPullerKeepsEverySource(t *testing.T) {
	older := &GrantCopy{Owner: "o@x", Revision: 3, Members: []GrantMember{{Email: "m@x", Circle: "team"}}}
	newer := &GrantCopy{Owner: "o@x", Revision: 7, Members: []GrantMember{{Email: "m@x", Circle: "team"}},
		Apps: map[string][]GrantRule{"app": {{Circles: []string{"team"}}}}}
	failNew := false
	a := grantSatellite(t, "ta", older, nil)
	b := grantSatellite(t, "tb", newer, &failNew)
	store := NewHostGrants("")
	p := &GrantPuller{Store: store, Sources: []GrantSource{
		{Name: "b", Endpoint: b.URL, Token: "tb"},
		{Name: "a", Endpoint: a.URL, Token: "ta"},
	}}
	p.PullOnce(context.Background())
	if got := store.Copy("b"); got == nil || got.Revision != 7 {
		t.Fatalf("copy b = %+v, want revision 7", got)
	}
	if got := store.Copy("a"); got == nil || got.Revision != 3 {
		t.Fatalf("copy a = %+v, want revision 3", got)
	}
	if !store.Allow("app", "m@x") {
		t.Fatal("member not admitted by the granting source's copy")
	}
	// A failed pull keeps the last good copy.
	failNew = true
	p.PullOnce(context.Background())
	if got := store.Copy("b"); got == nil || got.Revision != 7 {
		t.Fatalf("copy after failed pull = %+v, want revision 7", got)
	}
}

func TestRequireLoginGateUsesHostGrant(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	newFront := func(t *testing.T, store *HostGrants) *httptest.Server {
		reg := NewAppRegistry()
		for _, ac := range []conf.AppConfig{
			{Name: "guarded", Scheme: "http", Host: host, Port: port, Enabled: true, RequireLogin: true},
			{Name: "open", Scheme: "http", Host: host, Port: port, Enabled: true},
		} {
			if err := reg.RegisterFromConfig(ac); err != nil {
				t.Fatal(err)
			}
		}
		reg.SetHostGrants(store)
		front := httptest.NewServer(newTestRouter(reg))
		t.Cleanup(front.Close)
		return front
	}
	do := func(t *testing.T, front *httptest.Server, app, user string, cloud bool) int {
		req, _ := http.NewRequest("GET", front.URL+"/app/"+app+"/x", nil)
		if cloud {
			req.Header.Set("X-Forwarded-Prefix", "/matrix/h/h/app/"+app)
		}
		if user != "" {
			req.Header.Set("X-Periscope-User", user)
		}
		// The role is not consulted any more: a stamped role alone
		// must not open the gate.
		req.Header.Set("X-Periscope-Role", "admin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("no copy: owner only", func(t *testing.T) {
		front := newFront(t, NewHostGrants("o@x"))
		if got := do(t, front, "guarded", "O@x", true); got != http.StatusOK {
			t.Fatalf("owner = %d, want 200", got)
		}
		if got := do(t, front, "guarded", "m@x", true); got != http.StatusForbidden {
			t.Fatalf("member without copy = %d, want 403", got)
		}
		if got := do(t, front, "guarded", "", true); got != http.StatusForbidden {
			t.Fatalf("anonymous = %d, want 403", got)
		}
	})

	sat := grantSatellite(t, "tok", &GrantCopy{
		Host: "h", Owner: "o@x", Revision: 1,
		Members: []GrantMember{{Email: "m@x", Circle: "team"}, {Email: "g@x", Circle: "guest"}},
		Apps:    map[string][]GrantRule{"guarded": {{Circles: []string{"team"}}}},
	}, nil)
	store := NewHostGrants("")
	(&GrantPuller{Store: store, Sources: []GrantSource{{Endpoint: sat.URL, Token: "tok"}}}).PullOnce(context.Background())
	front := newFront(t, store)

	t.Run("member admitted by rule", func(t *testing.T) {
		if got := do(t, front, "guarded", "m@x", true); got != http.StatusOK {
			t.Fatalf("= %d, want 200", got)
		}
	})
	t.Run("member outside rule", func(t *testing.T) {
		if got := do(t, front, "guarded", "g@x", true); got != http.StatusForbidden {
			t.Fatalf("= %d, want 403", got)
		}
	})
	t.Run("owner from copy", func(t *testing.T) {
		if got := do(t, front, "guarded", "o@x", true); got != http.StatusOK {
			t.Fatalf("= %d, want 200", got)
		}
	})
	t.Run("LAN request unaffected", func(t *testing.T) {
		if got := do(t, front, "guarded", "", false); got != http.StatusOK {
			t.Fatalf("= %d, want 200", got)
		}
	})
	t.Run("require_login=false unaffected", func(t *testing.T) {
		if got := do(t, front, "open", "stranger@x", true); got != http.StatusOK {
			t.Fatalf("= %d, want 200", got)
		}
		if got := do(t, front, "open", "", true); got != http.StatusOK {
			t.Fatalf("anonymous = %d, want 200", got)
		}
	})
}

// countingSatellite serves *c behind a bearer and counts the pulls.
func countingSatellite(t *testing.T, c **GrantCopy, fail *bool, pulls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pulls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if *fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(*c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAllowFreshForcesOnePullOnDenial(t *testing.T) {
	ungranted := &GrantCopy{Owner: "o@x", Revision: 1, Members: []GrantMember{{Email: "m@x", Circle: "team"}}}
	granted := &GrantCopy{Owner: "o@x", Revision: 2, Members: []GrantMember{{Email: "m@x", Circle: "team"}},
		Apps: map[string][]GrantRule{"app": {{Circles: []string{"team"}}}}}
	setup := func(t *testing.T) (*HostGrants, **GrantCopy, *bool, *atomic.Int32) {
		cur, fail := ungranted, false
		var pulls atomic.Int32
		sat := countingSatellite(t, &cur, &fail, &pulls)
		store := NewHostGrants("")
		p := &GrantPuller{Store: store, Sources: []GrantSource{{Name: "s", Endpoint: sat.URL, Token: "tok"}}}
		p.PullOnce(context.Background())
		store.SetRefresh(p.PullOnce)
		pulls.Store(0)
		return store, &cur, &fail, &pulls
	}
	ctx := context.Background()

	t.Run("grant after last pull admitted at once", func(t *testing.T) {
		store, cur, _, pulls := setup(t)
		*cur = granted
		if !store.AllowFresh(ctx, "app", "m@x") {
			t.Fatal("newly granted member refused")
		}
		if n := pulls.Load(); n != 1 {
			t.Fatalf("pulls = %d, want 1", n)
		}
	})
	t.Run("non-member denials pull at most once per window", func(t *testing.T) {
		store, _, _, pulls := setup(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if store.AllowFresh(ctx, "app", "stranger@x") {
					t.Error("stranger admitted")
				}
			}()
		}
		wg.Wait()
		if store.AllowFresh(ctx, "app", "stranger@x") {
			t.Fatal("stranger admitted")
		}
		if n := pulls.Load(); n != 1 {
			t.Fatalf("pulls = %d, want 1", n)
		}
	})
	t.Run("owner never pulls", func(t *testing.T) {
		store, _, _, pulls := setup(t)
		if !store.AllowFresh(ctx, "app", "o@x") {
			t.Fatal("owner refused")
		}
		if store.AllowFresh(ctx, "app", "") {
			t.Fatal("anonymous admitted")
		}
		if n := pulls.Load(); n != 0 {
			t.Fatalf("pulls = %d, want 0", n)
		}
	})
	t.Run("pull failure keeps last copy and denies", func(t *testing.T) {
		store, cur, fail, pulls := setup(t)
		*cur, *fail = granted, true
		if store.AllowFresh(ctx, "app", "m@x") {
			t.Fatal("admitted despite failed pull")
		}
		if got := store.Copy("s"); got == nil || got.Revision != 1 {
			t.Fatalf("copy = %+v, want revision 1 kept", got)
		}
		if n := pulls.Load(); n != 1 {
			t.Fatalf("pulls = %d, want 1", n)
		}
	})
}

// TestHostGrantsRevisionIsAChangeTag: cloudbox's revision is a content
// hash, so a later copy may carry a SMALLER revision. It must still
// replace the held copy, or a grant, a revoke or an owner transfer that
// happens to hash lower is ignored until the daemon restarts.
func TestHostGrantsRevisionIsAChangeTag(t *testing.T) {
	store := NewHostGrants("o@x")
	ungranted := &GrantCopy{Owner: "o@x", Revision: 900, Members: []GrantMember{{Email: "m@x", Circle: "team"}}}
	granted := &GrantCopy{Owner: "o@x", Revision: 12, Members: ungranted.Members,
		Apps: map[string][]GrantRule{"app": {{Emails: []string{"m@x"}}}}}
	transferred := &GrantCopy{Owner: "n@x", Revision: 5, Members: ungranted.Members, Apps: granted.Apps}
	store.Update("s", ungranted)
	if store.Allow("app", "m@x") {
		t.Fatal("member admitted before the grant")
	}
	if !store.Update("s", granted) || !store.Allow("app", "m@x") {
		t.Fatal("a grant with a lower revision was ignored")
	}
	if store.Update("s", granted) {
		t.Fatal("an unchanged revision reported a change")
	}
	if !store.Update("s", transferred) || !store.Allow("app", "n@x") || store.Allow("app", "o@x") {
		t.Fatal("an owner transfer with a lower revision was ignored")
	}
	if !store.Update("s", ungranted) || store.Allow("app", "m@x") {
		t.Fatal("a revoke was ignored")
	}
}

// TestHostGrantsSourcesJudgedIndependently: two satellites hold different
// grants for the same attached host; neither hides the other's.
func TestHostGrantsSourcesJudgedIndependently(t *testing.T) {
	store := NewHostGrants("o@x")
	members := []GrantMember{{Email: "m@x", Circle: "team"}, {Email: "k@x", Circle: "team"}}
	store.Update("sat-a", &GrantCopy{Owner: "o@x", Revision: 1, Members: members,
		Apps: map[string][]GrantRule{"app": {{Emails: []string{"m@x"}}}}})
	store.Update("sat-b", &GrantCopy{Owner: "o@x", Revision: 2, Members: members})
	if !store.Allow("app", "m@x") || !store.Allow("app", "o@x") {
		t.Fatal("grantee or owner refused")
	}
	if store.Allow("app", "k@x") {
		t.Fatal("ungranted member admitted")
	}
	store.Update("sat-a", &GrantCopy{Owner: "o@x", Revision: 3, Members: members})
	if store.Allow("app", "m@x") {
		t.Fatal("revoke on the granting satellite ignored")
	}
}
