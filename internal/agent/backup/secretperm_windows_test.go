//go:build windows

package backup

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileAllAccess is FILE_ALL_ACCESS (0x1F01FF), what SDDL "FA" grants.
const fileAllAccess = 0x1F01FF

// assertSecretPerm is the Windows-native stand-in for the POSIX 0600
// assertion on the age identity file. The contract: the file's DACL
// is present, carries no inherited ACEs (inheritance was cut when the
// secret was written), and its only allow ACE grants the creating user
// FILE_ALL_ACCESS — no broad principal (Everyone, Users, Authenticated
// Users) and therefore no other local account may read the key.
func assertSecretPerm(t *testing.T, path string) {
	t.Helper()
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		t.Fatalf("open process token: %v", err)
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		t.Fatalf("token user: %v", err)
	}
	userSID := tu.User.Sid.String()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("%s: no DACL: %v — a secret without a DACL is world-accessible", path, err)
	}
	if dacl == nil {
		t.Fatalf("%s: DACL is present but empty, which Windows reads as fully permissive", path)
	}

	granted := map[string]uint32{}
	base := unsafe.Pointer(dacl)
	off := uintptr(8) // sizeof(ACL) header before the first ACE
	for i := uint16(0); i < dacl.AceCount; i++ {
		ace := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Add(base, off))
		off += uintptr(ace.Header.AceSize)
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			t.Errorf("%s: DACL has an inherited ACE; the secret must carry its own protected DACL", path)
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Add(unsafe.Pointer(ace), unsafe.Offsetof(ace.SidStart)))
		granted[sid.String()] |= uint32(ace.Mask)
	}
	if len(granted) != 1 || granted[userSID]&fileAllAccess != fileAllAccess {
		t.Errorf("%s: DACL trustees = %v, want exactly the creating user %s with FILE_ALL_ACCESS", path, granted, userSID)
	}
}
