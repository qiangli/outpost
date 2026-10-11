//go:build windows

package vknode

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// writePrivateTemp creates a credential file beside path with an owner-only
// DACL before writing any credential bytes. The caller publishes it by rename.
func writePrivateTemp(path string, data []byte) (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return "", fmt.Errorf("vknode: credential ACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	remove := func(e error) (string, error) { f.Close(); os.Remove(name); return "", e }
	if err := windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return remove(err)
	}
	if _, err := f.Write(data); err != nil {
		return remove(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
