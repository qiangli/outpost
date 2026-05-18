// Package hostauth verifies the host OS's own credentials. The application
// stores no passwords — every authentication goes through the OS subsystem
// (Open Directory on macOS, PAM on Linux, SAM on Windows) so the same
// password that unlocks the machine is what unlocks remote superadmin.
package hostauth

import (
	"errors"
	"os/user"
)

// ErrNotImplemented is returned by the stub OS implementations until real
// per-platform authenticators land (Linux PAM and Windows LogonUserW are
// follow-up tasks; v1 ships macOS only).
var ErrNotImplemented = errors.New("host auth not implemented on this OS")

// ErrInvalidCredentials is returned when the OS rejects the supplied
// username/password pair.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Authenticator verifies a username/password pair against the host OS.
// Implementations live in per-OS files (hostauth_<goos>.go).
type Authenticator interface {
	Authenticate(username, password string) error
}

// CurrentUser returns the agent process's own username — used as the
// canonical "this host's account". The matrix-agent runs as a user-level
// service, so this is the account the in-process shell will inherit.
func CurrentUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// CurrentDisplayName returns the GECOS / "full name" of the agent's OS
// user (e.g. "Alice Smith"). May be empty on stripped-down systems; the
// portal falls back to the username in that case.
func CurrentDisplayName() string {
	u, err := user.Current()
	if err != nil || u == nil {
		return ""
	}
	return u.Name
}

// StubAuth always returns the configured result. Tests inject this so we
// don't shell out to dscl during unit/integration tests.
type StubAuth struct {
	Want map[string]string // username → expected password
}

func (s StubAuth) Authenticate(user, pass string) error {
	if want, ok := s.Want[user]; ok && want == pass {
		return nil
	}
	return ErrInvalidCredentials
}
