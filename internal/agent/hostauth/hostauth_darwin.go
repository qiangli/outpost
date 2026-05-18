package hostauth

import (
	"errors"
	"os/exec"
)

// DefaultAuthenticator wraps macOS's Open Directory check via dscl —
// the same path Finder uses to validate the user's login password.
// No cgo; just exec.
func DefaultAuthenticator() Authenticator { return dsclAuth{} }

type dsclAuth struct{}

func (dsclAuth) Authenticate(user, pass string) error {
	if user == "" || pass == "" {
		return ErrInvalidCredentials
	}
	cmd := exec.Command("/usr/bin/dscl", ".", "-authonly", user, pass)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ErrInvalidCredentials
		}
		return err
	}
	return nil
}
