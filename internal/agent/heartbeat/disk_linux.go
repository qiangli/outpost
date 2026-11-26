package heartbeat

import "context"

func collectDisks(ctx context.Context) []measurement {
	return linuxMeasurements(ctx, "linux", "local", "Linux", "sh", "-c", diskScript)
}
