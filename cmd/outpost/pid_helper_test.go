//go:build !windows

package main

func exitZeroBin() string    { return "/bin/sh" }
func exitZeroArgs() []string { return []string{"-c", "exit 0"} }
