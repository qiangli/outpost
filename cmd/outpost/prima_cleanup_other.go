//go:build !windows

package main

// Unix permits replacing a running executable's directory entry. The
// existing post-provision cleanup handles old ranks holding fixed ports.
func stopStalePrimaForInstall(string) error { return nil }
