//go:build !linux

package runtimecmd

import "fmt"

func execMetrics(args []string) error {
	return fmt.Errorf("metrics-exec is only supported inside the Linux runtime image")
}
