package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/outpost/internal/agent/shard"
)

// primaReleaseBase is where the daemon fetches the prima engine binaries from —
// the fork's release, per-platform tarballs (prima-<goos>-<goarch>.tar.gz).
const primaReleaseBase = "https://github.com/qiangli/prima.cpp/releases/download/shard-binaries"

// expectedPrimaVersion is the VERSION marker shipped inside the release tarball.
// When a node's on-disk marker differs (or is absent), ensureBinaries re-fetches —
// this is what rolls a binary change (e.g. the ZMQ→nng MPL-free rebuild) out to
// nodes that already hold an older build instead of keeping the stale one.
const expectedPrimaVersion = "nng-3"

// provisionShard is the daemon's self-provisioner: it fetches the prima binaries
// and the model with NO human staging (over the daemon's own credentials), and
// returns the GGUF path prima loads. Idempotent — already-present binaries +
// models are reused. This is what dissolves the "needs ssh to stage" boundary.
func provisionShard(ctx context.Context, bins shard.ServeBins, modelName string, fwd shard.Forwarder, peers shard.PeerDiscoverer) (string, error) {
	if err := ensureBinaries(ctx, bins); err != nil {
		return "", fmt.Errorf("binaries: %w", err)
	}
	return ensureModel(ctx, "http://127.0.0.1:11434", modelName, fwd, peers)
}

// ensureBinaries downloads + extracts the prima release for this platform into
// the bins dir when the binaries are missing.
func ensureBinaries(ctx context.Context, bins shard.ServeBins) error {
	dir := filepath.Dir(bins.ServerBin)
	have := false
	if _, e1 := os.Stat(bins.ServerBin); e1 == nil {
		if _, e2 := os.Stat(bins.WorkerBin); e2 == nil {
			// Present — but re-fetch when the on-disk VERSION marker doesn't match,
			// so a binary change (e.g. the MPL-free nng rebuild) reaches nodes
			// holding an older build instead of keeping the stale one.
			v, _ := os.ReadFile(filepath.Join(dir, "VERSION"))
			have = strings.TrimSpace(string(v)) == expectedPrimaVersion
		}
	}
	if !have {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		asset := fmt.Sprintf("prima-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, primaReleaseBase+"/"+asset, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("fetch %s: %s", asset, resp.Status)
		}
		if err := extractPrima(resp.Body, dir); err != nil {
			return err
		}
	}
	// Ad-hoc sign on macOS — an unsigned arm64 Mach-O is SIGKILLed at exec by the
	// kernel, and a downloaded release artifact carries no signature valid for THIS
	// host. Applies whether freshly fetched or already on disk (a prior unsigned
	// fetch), so a worker self-provisioned before this fix gets repaired too.
	adhocSignDarwin(bins.ServerBin, bins.WorkerBin)
	return nil
}

// adhocSignDarwin ad-hoc code-signs the given binaries on macOS (no-op elsewhere)
// so a freshly-downloaded, unsigned prima binary can exec on Apple Silicon.
func adhocSignDarwin(paths ...string) {
	if runtime.GOOS != "darwin" {
		return
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		_ = exec.Command("codesign", "--force", "--sign", "-", p).Run()
	}
}

// extractPrima unpacks the llama-server + llama-cli executables from a tar.gz
// into dir (chmod 0755).
func extractPrima(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		base := filepath.Base(hdr.Name)
		switch base {
		case "llama-server", "llama-cli", "llama-server.exe", "llama-cli.exe", "VERSION":
		default:
			continue
		}
		out := filepath.Join(dir, base)
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
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
