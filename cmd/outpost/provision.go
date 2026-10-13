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

// Prima engine supply-chain gate (Sprint 436, story 625828548647; parent
// 17ba375e; prerequisite for the 8998b0dc live proof).
//
// Contract: first-party source tag bashy-prima-0.1.0 (peeled to
// f4ea9845dc91a18793921492e50ef23ec799e93e) built by bashy CI into the
// generated release bashy-bashy-prima-0.1.0-1, shipping raw executables
// prima-<goos>-<goarch> with SHA256SUMS and a per-asset .sigstore.json
// bundle, verified against the exact GitHub workflow identity and OIDC
// issuer declared in binmgr.
//
// Until that declaration exists with all four reviewed per-platform SHA pins
// (linux/darwin x amd64/arm64) and real bundles committed and verified,
// primaSignedTool refuses and the shard engine stays unavailable. Nothing
// here consults a legacy on-disk cache, $PATH, the old upstream
// shard-binaries release, or ad-hoc codesign.
const (
	primaSourceTag    = "bashy-prima-0.1.0"
	primaSourceCommit = "f4ea9845dc91a18793921492e50ef23ec799e93e"
	primaReleaseTag   = "bashy-bashy-prima-0.1.0-1"
	// primaReleaseRepo is the owner/repo hosting the generated release.
	// Empty until admitted with the yoke-side declaration (S436 90ea12d9).
	primaReleaseRepo = ""
	// primaWorkflowIdentity is the exact keyless signing identity (URI SAN —
	// a GitHub Actions workflow ref) that must have signed every asset.
	// Empty until admitted with the yoke-side declaration (S436 90ea12d9);
	// never a pattern, never a default.
	primaWorkflowIdentity = ""
)

// primaSHA256Pins holds the reviewed per-platform digests of the exact
// release assets binmgr downloads ("prima@<release>/<goos>/<goarch>"). Empty
// until all four are committed and verified — an absent pin is not an error
// elsewhere, but here it keeps the gate closed.
var primaSHA256Pins = map[string]string{}

// primaAssetName is the release asset for a platform: a raw executable.
func primaAssetName(goos, goarch string) string {
	return "prima-" + goos + "-" + goarch
}

// primaSignedTool builds the binmgr tool declaration for the admitted prima
// release. It refuses while any part of the admission is missing, so callers
// stay fail-closed without a separate flag. When yoke/pkg/binmgr declares
// EnsurePrima with this same contract, ensureBinaries switches to that call.
func primaSignedTool() (binmgr.Tool, error) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if primaWorkflowIdentity == "" {
		return binmgr.Tool{}, fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release for %s (workflow identity unadmitted)", platform)
	}
	if primaReleaseRepo == "" {
		return binmgr.Tool{}, fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release for %s (release repo unadmitted)", platform)
	}
	for _, plat := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		if primaSHA256Pins["prima@"+primaReleaseTag+"/"+plat] == "" {
			return binmgr.Tool{}, fmt.Errorf("prima engine unavailable: no admitted bashy-built, signed release for %s (SHA pin missing for %s)", platform, plat)
		}
	}
	goos, goarch, _ := strings.Cut(platform, "/")
	asset := primaAssetName(goos, goarch)
	return binmgr.Tool{
		Name:    "prima",
		Version: primaReleaseTag,
		Assets: map[string]binmgr.Asset{
			platform: {
				URL:    "https://github.com/" + primaReleaseRepo + "/releases/download/" + primaReleaseTag + "/" + asset,
				SHA256: primaSHA256Pins["prima@"+primaReleaseTag+"/"+platform],
			},
		},
		Sigstore: &binmgr.Sigstore{
			Identity: primaWorkflowIdentity,
			Issuer:   binmgr.GitHubActionsIssuer,
		},
	}, nil
}

// ensureBinaries provisions the shard engine binary exclusively through the
// signed binmgr tool: resolve the admitted declaration, verify + cache via
// binmgr.Ensure, then install the verified bytes for the required shard
// server (leader) and worker roles. Any admission gap refuses before any
// download, and no legacy cache, $PATH lookup, upstream release, or ad-hoc
// codesign is consulted on any path.
func ensureBinaries(ctx context.Context, bins shard.ServeBins) error {
	tool, err := primaSignedTool()
	if err != nil {
		return err
	}
	verified, err := binmgr.Ensure(ctx, tool)
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
