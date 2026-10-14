package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/binmgr"

	"github.com/qiangli/outpost/internal/agent/shard"
)

// provisionShard checks the signed prima engine gate before touching a local
// process or pulling a model. A legacy on-disk binary cannot bypass that gate.
func provisionShard(ctx context.Context, bins shard.ServeBins, ollamaURL, modelName string, fwd shard.Forwarder, peers shard.PeerDiscoverer) (string, error) {
	// Reap this outpost's own orphaned prima BEFORE the re-fetch. The async
	// launch detaches prima from the daemon ctx, so a torn-down or restarted
	// form can leave a worker/leader running — it holds the data ports AND,
	// on Windows, a lock on the binary, which wedges both the re-fetch's
	// replace and a fresh bind ("Address in use" / last_exit file lock).
	// Tracked PIDs only — never a pattern scan — on every platform.
	shard.KillTrackedPrima(bins.ServerBin)
	shard.KillTrackedPrima(bins.WorkerBin)
	if err := ensureBinaries(ctx, bins); err != nil {
		return "", fmt.Errorf("binaries: %w", err)
	}
	return ensureModel(ctx, ollamaURL, modelName, fwd, peers)
}

// Activation, peer provisioning and a local form can arrive together. Keep
// their cleanup and installation in one critical section so two provisioning
// calls cannot race while replacing the same role paths.
var primaInstallMu sync.Mutex

// ensureBinaries delegates release policy and verification to Yoke. Missing
// committed pins or invalid signatures fail closed, including on cache hits.
// Only verified paths may supply the leader and worker executables: the
// worker CLI through EnsurePrima, the leader server through
// EnsurePrimaServer — no download, no ad-hoc signing, no fallback. Windows
// additionally provisions the signed OpenMP runtime beside the executables.
func ensureBinaries(ctx context.Context, bins shard.ServeBins) error {
	primaInstallMu.Lock()
	defer primaInstallMu.Unlock()
	platform := runtime.GOOS + "/" + runtime.GOARCH
	worker, err := binmgr.EnsurePrima(ctx)
	if err != nil {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release for %s: %w", platform, err)
	}
	server, err := binmgr.EnsurePrimaServer(ctx)
	if err != nil {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release for %s: %w", platform, err)
	}
	if err := installVerifiedPrima(server, worker, bins); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		dll, err := binmgr.EnsurePrimaLibOMP(ctx)
		if err != nil {
			return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release for %s: %w", platform, err)
		}
		if err := installVerifiedLibOMP(dll, bins); err != nil {
			return err
		}
	}
	return nil
}

// installVerifiedPrima copies each binmgr-verified prima executable onto its
// shard role path (server = leader rank 0, worker = other ranks). The release
// ships a distinct CLI and server per platform, so each role runs its own
// verified bytes; each role path is still required and written explicitly.
func installVerifiedPrima(server, worker string, bins shard.ServeBins) error {
	return installVerifiedPrimaWithCleanup(server, worker, bins, stopStalePrimaForInstall)
}

// stopStalePrimaForInstall reaps the exact orphaned prima processes this
// outpost started from binPath (tracked PIDs only — never a pattern scan).
// It runs before either role destination is touched, so a stuck rank holding
// the binary lock cannot wedge the install. Always succeeds: entries are
// forgotten even when the PID is already gone.
func stopStalePrimaForInstall(binPath string) error {
	shard.KillTrackedPrima(binPath)
	return nil
}

func installVerifiedPrimaWithCleanup(server, worker string, bins shard.ServeBins, stop func(string) error) error {
	if strings.TrimSpace(server) == "" || strings.TrimSpace(worker) == "" {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release (empty verified path)")
	}
	if strings.TrimSpace(bins.ServerBin) == "" || strings.TrimSpace(bins.WorkerBin) == "" {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release (shard server/worker role paths are required)")
	}
	roles := []struct {
		verified, dst string
		data          []byte
	}{
		{verified: server, dst: bins.ServerBin},
		{verified: worker, dst: bins.WorkerBin},
	}
	for i := range roles {
		data, err := os.ReadFile(roles[i].verified)
		if err != nil {
			return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release: %w", err)
		}
		roles[i].data = data
	}
	// A running executable cannot be opened for overwrite on Windows, and a
	// prior daemon may have left a rank alive holding the lock. Reap this
	// outpost's own orphaned prima (tracked PIDs only — never a pattern
	// scan) before touching either destination. A refused cleanup leaves
	// both old role files intact.
	for _, role := range roles {
		if err := stop(role.dst); err != nil {
			return fmt.Errorf("prima install %s: stale process cleanup: %w", role.dst, err)
		}
	}
	// Rename-then-replace, never write-through: the install never writes
	// through a live file lock, on any platform.
	for _, role := range roles {
		if err := installFile(role.dst, role.data, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// installFile places data at dst without ever writing through a live file
// lock: bytes go to a temp file in the same directory, any live dst is
// renamed aside (a running image CAN be renamed — the loader opens it
// share-delete — even where it cannot be overwritten, which is exactly the
// stuck-Windows-prima wedge), the temp is renamed onto dst, and the aside
// backup is removed best-effort. A failed final rename rolls the old binary
// back. Uniform on every platform, so the Windows path is the tested path.
func installFile(dst string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("prima install %s: %w", dst, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".prima-install-*")
	if err != nil {
		return fmt.Errorf("prima install %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("prima install %s: %w", dst, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("prima install %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("prima install %s: %w", dst, err)
	}
	backup := ""
	if _, err := os.Stat(dst); err == nil {
		backup = dst + ".stale"
		_ = os.Remove(backup) // best-effort clear of a previous backup
		if err := os.Rename(dst, backup); err != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("prima install %s: %w", dst, err)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dst) // roll the old binary back
		}
		return fmt.Errorf("prima install %s: %w", dst, err)
	}
	if backup != "" {
		_ = os.Remove(backup) // best-effort; a stale aside is harmless
	}
	return nil
}

// installVerifiedLibOMP places the binmgr-verified Windows OpenMP runtime
// beside the shard executables — the .exe files load it from their own
// directory, so it sits next to each role path (deduped when both roles
// share a directory). Windows only; other platforms never call this.
func installVerifiedLibOMP(dll string, bins shard.ServeBins) error {
	if strings.TrimSpace(dll) == "" {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release (empty verified path)")
	}
	data, err := os.ReadFile(dll)
	if err != nil {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release: %w", err)
	}
	seen := map[string]bool{}
	for _, exe := range []string{bins.ServerBin, bins.WorkerBin} {
		dir := filepath.Dir(exe)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dst := filepath.Join(dir, binmgr.PrimaLibOMPFile)
		// Same rename-then-replace as the executables: a loaded DLL is
		// locked against overwrite on Windows too.
		if err := installFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ensureModel returns the GGUF blob path prima loads, fetching the model first
// only when the node doesn't already have it. It is three-tier:
//
//	(a) on disk already — resolve straight from the on-disk ollama manifest, NO
//	    running ollama daemon required, so a node that holds the GGUF can shard
//	    even with ollama down;
//	(b) a same-LAN mesh peer already has it — fetch the GGUF over the mesh
//	    forwarder (LAN-fast, no internet round-trip) instead of re-pulling;
//	(c) pull from the ollama registry (it verifies the digest — the anti-scam
//	    guarantee). This is the unchanged fallback when no peer has the model.
//
// fwd/peers may be nil (mesh / sharding not wired) — tier (b) is then skipped.
func ensureModel(ctx context.Context, ollamaURL, modelName string, fwd shard.Forwarder, peers shard.PeerDiscoverer) (string, error) {
	// (a) on disk already — no daemon needed.
	if path, err := resolveGGUF(modelName); err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			fmt.Fprintf(os.Stderr, "ensureModel: %q satisfied from local store\n", modelName)
			return path, nil
		}
	}
	// (b) fetch from a same-LAN mesh peer that already holds it.
	if fwd != nil && peers != nil {
		if lanPeers, err := peers.SameLANPeers(ctx); err == nil {
			for _, p := range lanPeers {
				if p.PeerID == "" {
					continue
				}
				got, ferr := fetchModelFromPeer(ctx, fwd, p.PeerID, modelName)
				if ferr != nil {
					fmt.Fprintf(os.Stderr, "ensureModel: peer %s transfer failed: %v\n", p.Host, ferr)
					continue
				}
				if !got {
					continue // peer doesn't have the model — try the next
				}
				if path, rerr := resolveGGUF(modelName); rerr == nil {
					if _, statErr := os.Stat(path); statErr == nil {
						fmt.Fprintf(os.Stderr, "ensureModel: %q fetched from peer %s\n", modelName, p.Host)
						return path, nil
					}
				}
			}
		}
	}
	// (c) pull from the ollama registry (the unchanged fallback).
	if strings.TrimSpace(ollamaURL) == "" {
		return "", fmt.Errorf("pull %q: no Ollama endpoint configured; enable or configure the ollama unit", modelName)
	}
	if err := ollamaPull(ctx, ollamaURL, modelName); err != nil {
		return "", fmt.Errorf("pull %q: %w", modelName, err)
	}
	fmt.Fprintf(os.Stderr, "ensureModel: %q pulled from ollama registry\n", modelName)
	return resolveGGUF(modelName)
}

// ollamaPull runs POST /api/pull to completion (ollama streams progress + a final
// status; stream=false collapses it to one response).
func ollamaPull(ctx context.Context, ollamaURL, name string) error {
	body, _ := json.Marshal(map[string]any{"name": name, "stream": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaURL+"/api/pull", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama pull %q: %s", name, resp.Status)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// resolveGGUF maps an ollama model name to its GGUF blob path via the manifest.
func resolveGGUF(name string) (string, error) {
	models := ollamaModelsDir()
	reg, ns, model, tag := parseModelRef(name)
	manifest := filepath.Join(models, "manifests", reg, ns, model, tag)
	data, err := os.ReadFile(manifest)
	if err != nil {
		return "", fmt.Errorf("manifest %s: %w", manifest, err)
	}
	var mf struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(data, &mf); err != nil {
		return "", err
	}
	for _, l := range mf.Layers {
		if strings.Contains(l.MediaType, ".model") {
			return filepath.Join(models, "blobs", "sha256-"+strings.TrimPrefix(l.Digest, "sha256:")), nil
		}
	}
	return "", fmt.Errorf("no model layer in manifest for %q", name)
}

func ollamaModelsDir() string {
	if d := os.Getenv("OLLAMA_MODELS"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ollama", "models")
}

// parseModelRef splits an ollama ref into registry/namespace/model/tag, applying
// the library defaults (registry.ollama.ai/library/<model>:latest).
func parseModelRef(name string) (reg, ns, model, tag string) {
	reg, ns, tag = "registry.ollama.ai", "library", "latest"
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i:], "/") {
		tag = name[i+1:]
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	switch len(parts) {
	case 1:
		model = parts[0]
	case 2:
		ns, model = parts[0], parts[1]
	default:
		reg, ns, model = parts[0], parts[1], strings.Join(parts[2:], "/")
	}
	return
}
