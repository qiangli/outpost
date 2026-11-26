package heartbeat

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"time"
)

const maxVolumes = 32
const maxGrowers = 3

type grower struct {
	Label string `json:"label"`
	Delta uint64 `json:"delta_bytes"`
}
type diskVolume struct {
	DisplayName string   `json:"display_name,omitempty"`
	Drive       string   `json:"drive,omitempty"`
	ID          string   `json:"id"`
	Used        uint64   `json:"used_bytes"`
	Total       uint64   `json:"total_bytes"`
	Growers     []grower `json:"growers,omitempty"`
}
type diskReport struct {
	At      time.Time    `json:"observed_at"`
	Volumes []diskVolume `json:"volumes"`
}

type measurement struct {
	diskVolume
	sizes map[string]uint64
}

func volumeID(kind, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%x", kind, sum[:8])
}
func (m measurement) report(previous measurement) diskVolume {
	v := m.diskVolume
	for label, size := range m.sizes {
		if before, ok := previous.sizes[label]; ok && size > before {
			v.Growers = append(v.Growers, grower{label, size - before})
		}
	}
	sort.Slice(v.Growers, func(i, j int) bool {
		if v.Growers[i].Delta == v.Growers[j].Delta {
			return v.Growers[i].Label < v.Growers[j].Label
		}
		return v.Growers[i].Delta > v.Growers[j].Delta
	})
	v.Growers = v.Growers[:min(len(v.Growers), maxGrowers)]
	return v
}

// A single background sampler publishes immutable snapshots. Retries carry
// the same observation time and deltas until there is a new complete sample.
func (w *Worker) sampleDisks(ctx context.Context) {
	var previous map[string]measurement
	for {
		started := time.Now().UTC()
		sampleCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		rows := collectDisks(sampleCtx)
		cancel()
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		next := make(map[string]measurement)
		report := diskReport{At: started}
		for _, m := range rows {
			if len(next) >= maxVolumes {
				break
			}
			if m.Total == 0 || m.Used > m.Total {
				continue
			}
			if _, found := next[m.ID]; found {
				continue
			}
			report.Volumes = append(report.Volumes, m.report(previous[m.ID]))
			next[m.ID] = m
		}
		previous = next
		w.disks.Store(&report)
		timer := time.NewTimer(w.cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
