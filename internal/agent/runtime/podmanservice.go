package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

var podmanServiceMu sync.Mutex

// EnsurePodmanService starts the already-provisioned Linux API service. It
// never installs binaries or resolves an operator-owned executable on PATH.
func EnsurePodmanService(ctx context.Context) (string, error) {
	if goruntime.GOOS != "linux" {
		return "", fmt.Errorf("managed podman API service requires Linux")
	}
	podmanServiceMu.Lock()
	defer podmanServiceMu.Unlock()
	bin := binmgr.CachedBinary("podman")
	if bin == "" {
		return "", fmt.Errorf("managed podman missing: provision it with bashy podman first")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "outpost", "podman")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ensureBundledPodmanConf(bin)
	return startPodmanService(ctx, bin, filepath.Join(dir, "api.sock"), 15*time.Second)
}

func podmanServiceReady(ctx context.Context, socket string) bool {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/_ping", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func startPodmanService(ctx context.Context, bin, socket string, timeout time.Duration) (string, error) {
	if podmanServiceReady(ctx, socket) {
		return socket, nil
	}
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, bin, "system", "service", "--time=0", "unix://"+socket)
	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start managed podman API: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); cancel() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return "", ctx.Err()
		case err := <-done:
			return "", fmt.Errorf("managed podman API exited before readiness: %v", err)
		case <-timer.C:
			cancel()
			<-done
			return "", fmt.Errorf("managed podman API readiness timed out")
		case <-ticker.C:
			if podmanServiceReady(ctx, socket) {
				return socket, nil
			}
		}
	}
}
