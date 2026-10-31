package ollama

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// WorkerRequestIDEnv is the ONE knob that marks this host as a provisioned
// cloud GPU worker: the worker bootstrapper exports it (in the daemon's
// service environment) with the provision request id the host was created
// for. Unset — every ordinary host — means the registry push carries no
// worker block at all.
const WorkerRequestIDEnv = "OUTPOST_GPU_WORKER_REQUEST_ID"

// Wire limits the master enforces on the worker block; a block outside them
// gets the WHOLE push rejected (HTTP 400), so the watcher checks them first
// and omits the block instead.
const (
	maxWorkerRequestIDLen = 64
	maxWorkerGPUModelLen  = 64
)

// errNoGPU is the fail-soft "nothing to measure" verdict.
var errNoGPU = errors.New("no NVIDIA GPU measurable (no /proc/driver/nvidia and no driver nvidia-smi)")

// WorkerInfo is the optional top-level "worker" object of a registry push,
// present only on a provisioned cloud GPU worker. RequestID is the provision
// request id (1-64 chars), VRAMBytes the total VRAM measured on the host,
// GPUModel the device name (<= 64 chars).
type WorkerInfo struct {
	RequestID string `json:"request_id"`
	VRAMBytes int64  `json:"vram_bytes"`
	GPUModel  string `json:"gpu_model"`
}

// WorkerRequestID reads WorkerRequestIDEnv. Empty means "not a worker".
func WorkerRequestID() string {
	return strings.TrimSpace(os.Getenv(WorkerRequestIDEnv))
}

// buildWorkerInfo validates a measured block against the wire limits. The
// GPU model is clamped (on a rune boundary) rather than refused — a long
// marketing name is not a reason to drop the whole block.
func buildWorkerInfo(requestID string, vramBytes int64, gpuModel string) (*WorkerInfo, error) {
	if n := len(requestID); n == 0 || n > maxWorkerRequestIDLen {
		return nil, fmt.Errorf("%s must be 1-%d chars (got %d)", WorkerRequestIDEnv, maxWorkerRequestIDLen, n)
	}
	if vramBytes < 0 {
		return nil, fmt.Errorf("negative vram_bytes %d", vramBytes)
	}
	gpuModel = strings.TrimSpace(gpuModel)
	for len(gpuModel) > maxWorkerGPUModelLen {
		_, size := utf8.DecodeLastRuneInString(gpuModel)
		gpuModel = gpuModel[:len(gpuModel)-size]
	}
	return &WorkerInfo{RequestID: requestID, VRAMBytes: vramBytes, GPUModel: strings.TrimSpace(gpuModel)}, nil
}

// workerHashTag mixes the worker block into the push content hash so the
// master's unchanged-snapshot fast path cannot skip persisting it (same
// reason clusterHashTag exists). Nil contributes nothing.
func workerHashTag(w *WorkerInfo) string {
	if w == nil {
		return ""
	}
	return fmt.Sprintf("|worker:%s:%d:%s", w.RequestID, w.VRAMBytes, w.GPUModel)
}

// MeasureNVIDIAGPU measures total VRAM and the device model of this host's
// NVIDIA GPUs. /proc/driver/nvidia gates it (no kernel driver → no GPU, no
// exec); the numbers come from nvidia-smi, which ships WITH the NVIDIA
// driver (no distro package). Multiple GPUs sum their VRAM; the model is the
// first device's name. Any failure is errNoGPU-wrapped so the caller omits
// the worker block.
func MeasureNVIDIAGPU(ctx context.Context) (vramBytes int64, gpuModel string, err error) {
	if _, err := os.Stat("/proc/driver/nvidia"); err != nil {
		return 0, "", errNoGPU
	}
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		bin = filepath.Join("/usr", "bin", "nvidia-smi")
		if _, serr := os.Stat(bin); serr != nil {
			return 0, "", fmt.Errorf("%w: nvidia-smi not found", errNoGPU)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin,
		"--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, "", fmt.Errorf("%w: nvidia-smi: %v", errNoGPU, err)
	}
	return parseNvidiaSmi(string(out))
}

// parseNvidiaSmi parses `name,memory.total` CSV rows (memory in MiB).
func parseNvidiaSmi(out string) (vramBytes int64, gpuModel string, err error) {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		i := strings.LastIndex(line, ",")
		if i < 0 {
			continue
		}
		mib, perr := strconv.ParseInt(strings.TrimSpace(line[i+1:]), 10, 64)
		if perr != nil || mib <= 0 {
			continue
		}
		vramBytes += mib << 20
		if gpuModel == "" {
			gpuModel = strings.TrimSpace(line[:i])
		}
	}
	if vramBytes == 0 {
		return 0, "", fmt.Errorf("%w: nvidia-smi reported no GPU memory", errNoGPU)
	}
	return vramBytes, gpuModel, nil
}
