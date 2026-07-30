//go:build windows

package main

func exitZeroBin() string    { return "cmd.exe" }
func exitZeroArgs() []string { return []string{"/c", "exit 0"} }
