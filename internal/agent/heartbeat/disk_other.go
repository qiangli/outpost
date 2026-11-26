//go:build !windows && !linux

package heartbeat

import "context"

func collectDisks(context.Context) []measurement { return nil }
