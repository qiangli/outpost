//go:build windows

package machineid

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// osMachineID reads the MachineGuid from HKLM\SOFTWARE\Microsoft\Cryptography on Windows.
func osMachineID() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		k, err = registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
		if err != nil {
			return "", err
		}
	}
	defer k.Close()

	guid, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", err
	}
	guid = strings.TrimSpace(guid)
	if guid == "" {
		return "", errors.New("empty MachineGuid in registry")
	}
	return guid, nil
}
