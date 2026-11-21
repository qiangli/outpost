//go:build !linux

package heartbeat

import "context"

func WatchPower(context.Context) <-chan PowerObservation { return nil }
