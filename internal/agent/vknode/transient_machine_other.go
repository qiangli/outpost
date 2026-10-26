//go:build !windows

package vknode

import "context"

func podmanTransientHost(string) func(context.Context) string { return nil }
