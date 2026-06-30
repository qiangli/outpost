package main

import (
	"testing"
	"time"
)

// TestKeepAliveIntervalHasSlideMargin guards the elevation-cookie expiry fix.
// Cloudbox slides the 1 h idle cookie only when LESS THAN 30 min remain (its
// ElevationTTL/2), so the keep-alive ping must land in that second-half window
// with margin. Pinging AT exactly 30 min rode the boundary and missed the slide
// under timing jitter, which forced needless re-elevations around each hour.
// Keep the cadence comfortably below the threshold.
func TestKeepAliveIntervalHasSlideMargin(t *testing.T) {
	const cloudboxSlideThreshold = 30 * time.Minute // mirrors cloudbox ElevationTTL/2
	if keepAliveInterval >= cloudboxSlideThreshold {
		t.Fatalf("keepAliveInterval=%s must be < the cloudbox slide threshold %s — pinging at/above it rides the boundary and misses the slide",
			keepAliveInterval, cloudboxSlideThreshold)
	}
	if margin := cloudboxSlideThreshold - keepAliveInterval; margin < 10*time.Minute {
		t.Fatalf("keepAliveInterval=%s leaves only %s of margin below the %s slide threshold; want >= 10m so a delayed ping still slides",
			keepAliveInterval, margin, cloudboxSlideThreshold)
	}
}
