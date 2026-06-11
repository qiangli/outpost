package hostauth

import "strings"

// BareUsername strips platform qualifiers from an account name so the
// same human-typed username works against every OS:
//
//   - "MACHINE\alice" / "DOMAIN\alice" (Windows down-level) → "alice"
//   - "alice@example.com"              (Windows UPN)        → "alice"
//   - "alice"                          (Unix / bare)        → "alice"
//
// Unix account names cannot contain '\' or '@' (useradd and macOS both
// forbid them), and Windows SAM account names forbid both characters
// too, so stripping at the last '\' / first '@' is unambiguous: any
// qualifier present is exactly one of the two Windows forms.
func BareUsername(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	} else if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	return s
}

// SameUser reports whether a submitted username refers to the same
// account as the canonical one, accepting either the exact local form
// or the bare (unqualified) form, case-insensitively. This is the
// single equality rule for the "submitted user must be the agent's
// own OS user" gates (/auth and the SSH PasswordCallback).
//
// On Windows, Go's user.Current().Username is the SAM-compatible
// "MACHINE\user" — demanding that exact string from an SSH client is
// hostile (quoting a backslash plus a space), so "user" alone must
// match too. The match only widens the *comparison*: callers still
// authenticate the canonical name, so the password remains the gate.
func SameUser(submitted, canonical string) bool {
	submitted = strings.TrimSpace(submitted)
	canonical = strings.TrimSpace(canonical)
	if submitted == "" || canonical == "" {
		return false
	}
	if strings.EqualFold(submitted, canonical) {
		return true
	}
	return strings.EqualFold(BareUsername(submitted), BareUsername(canonical))
}
