//go:build windows

package vknode

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("credential file requires owner-only DACL: %v", err)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("credential DACL has %d ACEs, want one owner grant", dacl.AceCount)
	}
	ace := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Add(unsafe.Pointer(dacl), 8))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		t.Fatalf("credential DACL has unexpected ACE type or inherited grant")
	}
	sid := (*windows.SID)(unsafe.Add(unsafe.Pointer(ace), unsafe.Offsetof(ace.SidStart)))
	if sid.String() != user.User.Sid.String() || uint32(ace.Mask)&0x1f01ff != 0x1f01ff {
		t.Fatalf("credential DACL must grant file access only to the current user")
	}
}
