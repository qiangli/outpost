//go:build linux

package hostauth

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/GehirnInc/crypt"
	_ "github.com/GehirnInc/crypt/md5_crypt"
	_ "github.com/GehirnInc/crypt/sha256_crypt"
	_ "github.com/GehirnInc/crypt/sha512_crypt"
)

// DefaultAuthenticator on Linux verifies the OS password WITHOUT cgo or
// libpam. It shells out to the setuid unix_chkpwd PAM helper and, if that
// rejects a valid password, falls back to reading /etc/shadow directly and
// verifying the stored crypt(3) hash in pure Go.
//
// Why not cgo PAM (the previous implementation)? Outpost release binaries
// are cross-compiled with CGO_ENABLED=0 (scripts/build-all.sh), so the cgo
// PAM authenticator never linked in practice — the host fell through to the
// no-cgo stub that returned ErrNotImplemented and rejected *every* password,
// which is why a freshly-paired Linux host could not be connected to at all.
// A pure-Go implementation means the standard cross-compiled binary
// authenticates correctly with no libpam-dev build dependency.
//
// Why the /etc/shadow fallback? Ubuntu 26.04 ships PAM 1.7.0 with a
// unix_chkpwd helper that rejects otherwise-valid passwords; reading the
// shadow hash ourselves sidesteps it. The fallback requires the outpost
// process to be able to read /etc/shadow (shadow-group membership, or
// running as root) — when it can't, only the unix_chkpwd path is available.
func DefaultAuthenticator() Authenticator { return linuxAuth{} }

type linuxAuth struct{}

func (linuxAuth) Authenticate(user, pass string) error {
	// Guard empty credentials up front: unix_chkpwd's "nullok" mode would
	// otherwise accept an empty password against a passwordless account.
	if user == "" || pass == "" {
		return ErrInvalidCredentials
	}
	if err := authChkpwd(user, pass); err == nil {
		return nil
	}
	return authShadow(user, pass)
}

// authChkpwd shells out to unix_chkpwd, the standard setuid PAM helper, so
// no cgo or libpam linkage is needed. The password is fed on stdin.
func authChkpwd(username, password string) error {
	var chkpwd string
	for _, p := range []string{"/sbin/unix_chkpwd", "/usr/sbin/unix_chkpwd"} {
		if _, err := exec.LookPath(p); err == nil {
			chkpwd = p
			break
		}
	}
	if chkpwd == "" {
		var err error
		if chkpwd, err = exec.LookPath("unix_chkpwd"); err != nil {
			return ErrInvalidCredentials
		}
	}

	cmd := exec.Command(chkpwd, username, "nullok")
	cmd.Stdin = strings.NewReader(password)
	if err := cmd.Run(); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// authShadow reads /etc/shadow directly and verifies the password against
// the stored crypt(3) hash. Requires shadow-group membership (or root) to
// open the file; when it can't, this returns ErrInvalidCredentials and the
// unix_chkpwd path is the only one that can succeed.
func authShadow(username, password string) error {
	f, err := os.Open("/etc/shadow")
	if err != nil {
		return ErrInvalidCredentials
	}
	defer f.Close()
	return verifyShadow(f, username, password)
}

// verifyShadow holds the shadow-file parsing + crypt verification, split
// from authShadow's file I/O so it can be tested against an in-memory
// shadow snippet without root.
func verifyShadow(r io.Reader, username, password string) error {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ":", 3)
		if len(parts) < 2 || parts[0] != username {
			continue
		}
		hash := parts[1]
		// Locked / no-password accounts: "!" / "*" prefixes, or empty.
		if hash == "" || hash[0] == '!' || hash[0] == '*' {
			return ErrInvalidCredentials
		}
		c := crypt.NewFromHash(hash)
		if c == nil {
			return ErrInvalidCredentials
		}
		if err := c.Verify(hash, []byte(password)); err != nil {
			return ErrInvalidCredentials
		}
		return nil
	}
	return ErrInvalidCredentials
}
