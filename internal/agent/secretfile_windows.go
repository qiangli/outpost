//go:build windows

package agent

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictSecretFile enforces the Windows-native half of the 0600
// confidentiality contract for outpost's secret files (the ssh host
// key and the outbound matrix cookies). Go's os.OpenFile maps the
// mode only to the read-only attribute on Windows, so a plain create
// inherits the parent directory DACL — under a broadly readable
// parent that leaves a bearer-equivalent credential readable by
// other local accounts. This sets a protected DACL whose single ACE
// grants FILE_ALL_ACCESS to the creating user, the closest native
// equivalent of owner-only 0600.
//
// Deliberately not a general ACL layer: one grant, one file, called
// from the secret write paths.
func restrictSecretFile(path string) error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("open process token: %w", err)
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("token user: %w", err)
	}
	sid := tu.User.Sid.String()
	if sid == "" {
		return fmt.Errorf("current user SID unavailable")
	}
	// D:P = protected DACL (inheritance cut); (A;;FA;;;SID) = one
	// allow ACE, FILE_ALL_ACCESS, for that user alone.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return fmt.Errorf("build security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("security descriptor DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("set %s DACL: %w", path, err)
	}
	return nil
}
