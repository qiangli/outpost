package hostauth

import (
	"errors"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DefaultAuthenticator on Windows calls advapi32!LogonUserW to verify
// credentials against the local SAM (LOGON32_LOGON_NETWORK,
// LOGON32_PROVIDER_DEFAULT). x/sys/windows doesn't export LogonUser, so
// we load advapi32 via LazyDLL — still pure Go, no cgo.
//
// Username forms:
//   - "alice"            → local account on the current machine.
//   - "alice@example"    → UPN; domain inferred from the suffix.
//   - "DOMAIN\\alice"    → split on the backslash.
func DefaultAuthenticator() Authenticator { return logonAuth{} }

type logonAuth struct{}

const (
	logon32LogonNetwork    = 3
	logon32ProviderDefault = 0
)

var (
	modAdvapi32    = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW = modAdvapi32.NewProc("LogonUserW")
)

func (logonAuth) Authenticate(user, pass string) error {
	if user == "" || pass == "" {
		return ErrInvalidCredentials
	}
	domain := "."
	if i := strings.IndexByte(user, '\\'); i >= 0 {
		domain, user = user[:i], user[i+1:]
	} else if i := strings.IndexByte(user, '@'); i >= 0 {
		domain, user = user[i+1:], user[:i]
	}

	uPtr, err := syscall.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	dPtr, err := syscall.UTF16PtrFromString(domain)
	if err != nil {
		return err
	}
	pPtr, err := syscall.UTF16PtrFromString(pass)
	if err != nil {
		return err
	}

	var token windows.Handle
	r1, _, callErr := procLogonUserW.Call(
		uintptr(unsafe.Pointer(uPtr)),
		uintptr(unsafe.Pointer(dPtr)),
		uintptr(unsafe.Pointer(pPtr)),
		uintptr(logon32LogonNetwork),
		uintptr(logon32ProviderDefault),
		uintptr(unsafe.Pointer(&token)),
	)
	if r1 == 0 {
		if errors.Is(callErr, windows.ERROR_LOGON_FAILURE) ||
			errors.Is(callErr, windows.ERROR_ACCOUNT_RESTRICTION) ||
			errors.Is(callErr, windows.ERROR_INVALID_LOGON_HOURS) ||
			errors.Is(callErr, windows.ERROR_PASSWORD_EXPIRED) {
			return ErrInvalidCredentials
		}
		return callErr
	}
	_ = windows.CloseHandle(token)
	return nil
}
