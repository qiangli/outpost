//go:build !windows

package vknode

import "context"

func podmanTransientHost(string) func(context.Context, int) string { return nil }
