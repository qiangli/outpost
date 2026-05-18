//go:build linux && cgo

package hostauth

import (
	"errors"
	"fmt"
	"os"

	"github.com/msteinert/pam/v2"
)

// DefaultAuthenticator on Linux uses PAM. The service name defaults to
// "login" but is overridable with MATRIX_PAM_SERVICE for distros that
// expose a more specific auth-only stack (e.g. "common-auth",
// "system-auth"). Build with CGO_ENABLED=1 and libpam-dev installed.
func DefaultAuthenticator() Authenticator {
	service := os.Getenv("MATRIX_PAM_SERVICE")
	if service == "" {
		service = "login"
	}
	return pamAuth{service: service}
}

type pamAuth struct {
	service string
}

func (a pamAuth) Authenticate(user, pass string) error {
	if user == "" || pass == "" {
		return ErrInvalidCredentials
	}
	t, err := pam.StartFunc(a.service, user, func(s pam.Style, _ string) (string, error) {
		if s == pam.PromptEchoOff {
			return pass, nil
		}
		return "", nil
	})
	if err != nil {
		return fmt.Errorf("pam start: %w", err)
	}
	defer t.End()
	if err := t.Authenticate(0); err != nil {
		if errors.Is(err, pam.ErrAuth) || errors.Is(err, pam.ErrCred) || errors.Is(err, pam.ErrPermDenied) {
			return ErrInvalidCredentials
		}
		return err
	}
	return nil
}
