package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"

	"github.com/qiangli/outpost/internal/agent/shard"
)

// provisionShard checks the signed prima engine gate before touching a local
// process or pulling a model. A legacy on-disk binary cannot bypass that gate.
func provisionShard(ctx context.Context, bins shard.ServeBins, ollamaURL, modelName string, fwd shard.Forwarder, peers shard.PeerDiscoverer) (string, error) {
	// Kill any orphaned prima from a previous form before re-provisioning. The
	// async launch detaches prima from the daemon ctx, so a torn-down or
	// restarted form can leave a worker/leader running — it holds the data ports
	// AND the binary inode, which blocks both the re-fetch's overwrite and a
	// fresh bind ("Address in use"). Targeted: only this shard's own prima
	// binaries by exact path, never the daemon, never a loose pattern.
	if err := ensureBinaries(ctx, bins); err != nil {
		return "", fmt.Errorf("binaries: %w", err)
	}
	killStalePrima(bins.ServerBin)
	killStalePrima(bins.WorkerBin)
	return ensureModel(ctx, ollamaURL, modelName, fwd, peers)
}

// killStalePrima SIGKILLs any process whose command line contains exactly this
// prima binary path — an orphan from a prior form. Unix-only (pgrep); a no-op on
// Windows and when nothing matches. It only ever targets the shard's own compute
// children (the prima binary by exact path), never the outpost daemon.
func killStalePrima(binPath string) {
	if binPath == "" || runtime.GOOS == "windows" {
		return
	}
	out, err := exec.Command("pgrep", "-f", binPath).Output()
	if err != nil {
		return // pgrep exits non-zero when nothing matches
	}
	self := os.Getpid()
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid == self {
			continue
		}
		if p, e := os.FindProcess(pid); e == nil {
			_ = p.Kill()
		}
	}
}

// ensureBinaries delegates release policy and verification to Yoke. Missing
// committed pins or invalid signatures fail closed, including on cache hits.
// Only the verified path may supply the leader and worker executables.
func ensureBinaries(ctx context.Context, bins shard.ServeBins) error {
	verified, err := binmgr.EnsurePrima(ctx)
	if err != nil {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release for %s/%s: %w", runtime.GOOS, runtime.GOARCH, err)
	}
	return installVerifiedPrima(verified, bins)
}

// installVerifiedPrima copies the binmgr-verified prima binary onto both
// required shard role paths (server = leader rank 0, worker = other ranks).
// The release ships one executable per platform, so both roles run the same
// verified bytes; each role path is still required and written explicitly.
func installVerifiedPrima(verified string, bins shard.ServeBins) error {
	if strings.TrimSpace(verified) == "" {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release (empty verified path)")
	}
	if strings.TrimSpace(bins.ServerBin) == "" || strings.TrimSpace(bins.WorkerBin) == "" {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release (shard server/worker role paths are required)")
	}
	data, err := os.ReadFile(verified)
	if err != nil {
		return fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release: %w", err)
	}
	for _, dst := range []string{bins.ServerBin, bins.WorkerBin} {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("prima install %s: %w", dst, err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return fmt.Errorf("prima install %s: %w", dst, err)
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
