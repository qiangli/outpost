package clusterllm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// aggregate is the distilled result of querying a backend's management API
// for its worker/GPU inventory.
type aggregate struct {
	members   int
	vramBytes uint64
	version   string
}

// aggregateGPUStack queries GPUStack's management API for the cluster's
// worker count and summed GPU memory. Best-effort and key-gated: returns
// ok=false on any failure (no key, auth rejected, path drift across
// versions, decode error), which the caller treats as "unknown" and leaves
// the cloudbox size filter inert.
//
// GPUStack's management API is Bearer-gated and versioned its path prefix
// over releases (v0.6 uses /v2; earlier used /v1), so we try both. The
// gpu-devices view flattens every worker's accelerators into one list with
// a per-device memory object; we sum total bytes and count distinct
// workers. Apple-Silicon unified memory reports as a single large total —
// summing across member Macs is an acceptable upper bound for the
// "can this cluster hold an N-byte model" question the filter answers.
func aggregateGPUStack(ctx context.Context, client *http.Client, endpoint, apiKey string) (aggregate, bool) {
	if strings.TrimSpace(apiKey) == "" {
		return aggregate{}, false
	}
	var out aggregate
	out.version = probeVersion(ctx, client, endpoint, apiKey)

	devices, ok := listGPUDevices(ctx, client, endpoint, apiKey)
	if !ok {
		return aggregate{}, false
	}
	workers := map[string]struct{}{}
	for _, d := range devices {
		out.vramBytes += d.totalBytes()
		if id := d.workerKey(); id != "" {
			workers[id] = struct{}{}
		}
	}
	out.members = len(workers)
	return out, true
}

// gpuDevice is a tolerant decode of one entry in GPUStack's gpu-devices
// view. Field names are matched defensively across versions: memory may be
// a nested object ({total}) or a flattened scalar (memory_total). Only the
// fields we need are decoded; everything else is ignored.
type gpuDevice struct {
	WorkerID    json.Number `json:"worker_id"`
	WorkerName  string      `json:"worker_name"`
	Memory      *gpuMemory  `json:"memory"`
	MemoryTotal json.Number `json:"memory_total"`
}

type gpuMemory struct {
	Total           json.Number `json:"total"`
	IsUnifiedMemory bool        `json:"is_unified_memory"`
}

func (d gpuDevice) totalBytes() uint64 {
	if d.Memory != nil {
		if n, err := d.Memory.Total.Int64(); err == nil && n > 0 {
			return uint64(n)
		}
	}
	if n, err := d.MemoryTotal.Int64(); err == nil && n > 0 {
		return uint64(n)
	}
	return 0
}

func (d gpuDevice) workerKey() string {
	if s := strings.TrimSpace(d.WorkerName); s != "" {
		return s
	}
	return strings.TrimSpace(d.WorkerID.String())
}

// listResponse is GPUStack's standard paginated list envelope. We request a
// large page so a single call covers any realistic home cluster.
type listResponse[T any] struct {
	Items []T `json:"items"`
}

func listGPUDevices(ctx context.Context, client *http.Client, endpoint, apiKey string) ([]gpuDevice, bool) {
	for _, prefix := range []string{"/v2", "/v1"} {
		body, ok := getJSON(ctx, client, endpoint+prefix+"/gpu-devices?perPage=1000", apiKey)
		if !ok {
			continue
		}
		var lr listResponse[gpuDevice]
		if err := json.Unmarshal(body, &lr); err != nil {
			continue
		}
		// A successful decode with zero items is still authoritative (an
		// idle/headless cluster) — return it rather than falling through.
		return lr.Items, true
	}
	return nil, false
}

// probeVersion best-effort reads the backend version banner. Empty on any
// failure — version is cosmetic (admin UI + staleness), never load-bearing.
func probeVersion(ctx context.Context, client *http.Client, endpoint, apiKey string) string {
	for _, path := range []string{"/v1/version", "/version"} {
		body, ok := getJSON(ctx, client, endpoint+path, apiKey)
		if !ok {
			continue
		}
		var v struct {
			Version string `json:"version"`
			Git     string `json:"git_version"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			continue
		}
		if v.Version != "" {
			return v.Version
		}
		if v.Git != "" {
			return v.Git
		}
	}
	return ""
}

// getJSON GETs a Bearer-authed JSON endpoint with the package probe
// timeout. Returns ok=false on transport error or any non-2xx so callers
// fall through to the next candidate path / inert fallback.
func getJSON(ctx context.Context, client *http.Client, url, apiKey string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	return body, true
}
