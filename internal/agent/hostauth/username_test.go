package hostauth

import "testing"

func TestBareUsername(t *testing.T) {
	cases := []struct{ in, want string }{
		{"alice", "alice"},
		{"  alice  ", "alice"},
		{`HOST-F\Lijuan Song`, "Lijuan Song"},
		{`DOMAIN\alice`, "alice"},
		{"alice@example.com", "alice"},
		{"", ""},
	}
	for _, c := range cases {
		if got := BareUsername(c.in); got != c.want {
			t.Errorf("BareUsername(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSameUser(t *testing.T) {
	cases := []struct {
		submitted, canonical string
		want                 bool
	}{
		// Exact + case-folded.
		{"alice", "alice", true},
		{"Alice", "alice", true},
		// Bare form matches the Windows SAM-compatible canonical —
		// the cross-platform consistency this helper exists for.
		{"Lijuan Song", `HOST-F\Lijuan Song`, true},
		{"lijuan song", `HOST-F\Lijuan Song`, true},
		{`host-f\lijuan song`, `HOST-F\Lijuan Song`, true},
		// UPN form.
		{"alice@example.com", `EXAMPLE\alice`, true},
		// Qualified submitted vs bare canonical (Unix daemon, client
		// pasted a Windows-style name): bare parts must still match.
		{`SOMEBOX\alice`, "alice", true},
		// Mismatches.
		{"bob", `HOST-F\Lijuan Song`, false},
		{`HOST-F\bob`, `HOST-F\Lijuan Song`, false},
		{"", "alice", false},
		{"alice", "", false},
	}
	for _, c := range cases {
		if got := SameUser(c.submitted, c.canonical); got != c.want {
			t.Errorf("SameUser(%q, %q) = %v, want %v", c.submitted, c.canonical, got, c.want)
		}
	}
}
