//go:build !windows

package backup

// restrictSecretFile is a no-op where the 0600 mode passed to
// os.OpenFile already carries the confidentiality contract.
func restrictSecretFile(string) error { return nil }
