//go:build linux && !cgo

package hostauth

// On Linux without cgo we cannot link libpam. Build with CGO_ENABLED=1
// and have libpam-dev (or equivalent) installed to get the real PAM
// implementation; this stub keeps `CGO_ENABLED=0 go build` working and
// makes the limitation explicit at runtime.
func DefaultAuthenticator() Authenticator { return notImplementedAuth{} }

type notImplementedAuth struct{}

func (notImplementedAuth) Authenticate(string, string) error { return ErrNotImplemented }
