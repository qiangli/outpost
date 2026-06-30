package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/outpost/internal/agent/shard"
)

// modelBlobsService is the mesh-forwarder service name under which a node serves
// its on-disk ollama model store (manifests + blobs) read-only, so a same-LAN
// peer can fetch a model's GGUF it already has instead of re-pulling from the
// ollama registry. The constant is shared between the exposure side
// (serveModelBlobs) and the client (fetchModelFromPeer) — they must agree.
const modelBlobsService = "model-blobs"

// transferLoopback is this package's loopback host (mirrors shard.loopback,
// which is unexported in the shard package).
const transferLoopback = "127.0.0.1"

// modelBlobsHandler serves THIS node's ollama model store read-only over the
// mesh: /has, /manifest, /blob. Path construction mirrors resolveGGUF so a
// transfer lands exactly where resolveGGUF later resolves.
func modelBlobsHandler() http.Handler {
	mux := http.NewServeMux()
	// /has?model=<name> → {"has":bool}: true iff the GGUF resolves AND is on disk.
	mux.HandleFunc("/has", func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		has := false
		if path, err := resolveGGUF(model); err == nil {
			if _, statErr := os.Stat(path); statErr == nil {
				has = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"has": has})
	})
	// /manifest?model=<name> → raw manifest file bytes.
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		if model == "" {
			http.Error(w, "missing model", http.StatusBadRequest)
			return
		}
		reg, ns, name, tag := parseModelRef(model)
		manifest := filepath.Join(ollamaModelsDir(), "manifests", reg, ns, name, tag)
		data, err := os.ReadFile(manifest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	// /blob?digest=<hex> → stream blobs/sha256-<hex>. The digest MUST be pure
	// lowercase hex — the guard that keeps it from escaping the blobs dir.
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		digest := r.URL.Query().Get("digest")
		if !isHexDigest(digest) {
			http.Error(w, "invalid digest", http.StatusBadRequest)
			return
		}
		blob := filepath.Join(ollamaModelsDir(), "blobs", "sha256-"+digest)
		f, err := os.Open(blob)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, f)
	})
	return mux
}

// isHexDigest reports whether s is a non-empty pure lowercase-hex string — the
// anti-path-traversal guard for /blob's digest (no separators, no "..").
func isHexDigest(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// serveModelBlobs runs the model-blobs handler on a fresh loopback listener and
// exposes it over the mesh as modelBlobsService, so a same-LAN peer can fetch a
// model's GGUF from this node. Mirrors shard.ServeControl. The returned cleanup
// unexposes the service + shuts the server down.
func serveModelBlobs(fwd shard.Forwarder) (func(), error) {
	ln, err := net.Listen("tcp", transferLoopback+":0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: modelBlobsHandler()}
	go func() { _ = srv.Serve(ln) }()
	fwd.Expose(modelBlobsService, ln.Addr().String())
	return func() {
		fwd.Unexpose(modelBlobsService)
		_ = srv.Close()
	}, nil
}

// fetchModelFromPeer pulls modelName's manifest + blobs from a same-LAN mesh
// peer over the model-blobs service, writing them into THIS node's ollama store
// so resolveGGUF then resolves locally. Returns (false,nil) when the peer
// doesn't hold the model (the caller falls through to the next peer / registry
// pull); (true,nil) only on a complete transfer. On any mid-transfer error it
// removes the blobs it wrote and returns the error.
func fetchModelFromPeer(ctx context.Context, fwd shard.Forwarder, peerID, modelName string) (bool, error) {
	ln, err := fwd.Listen(transferLoopback+":0", peerID, modelBlobsService)
	if err != nil {
		return false, err
	}
	defer ln.Close()
	base := "http://" + ln.Addr().String()

	// /has — skip the peer when it doesn't hold the model.
	has, err := peerHasModel(ctx, base, modelName)
	if err != nil {
		return false, err
	}
	if !has {
		return false, nil
	}

	// /manifest — the index of blobs to fetch.
	manifestData, err := getBytes(ctx, base+"/manifest?model="+url.QueryEscape(modelName))
	if err != nil {
		return false, err
	}
	var mf struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestData, &mf); err != nil {
		return false, err
	}

	models := ollamaModelsDir()
	blobsDir := filepath.Join(models, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		return false, err
	}

	// Unique digests = config + every layer (skip ones already on disk).
	var digests []string
	seen := map[string]bool{}
	add := func(d string) {
		hex := strings.TrimPrefix(d, "sha256:")
		if hex != "" && !seen[hex] {
			seen[hex] = true
			digests = append(digests, hex)
		}
	}
	add(mf.Config.Digest)
	for _, l := range mf.Layers {
		add(l.Digest)
	}

	var written []string
	cleanup := func() {
		for _, p := range written {
			_ = os.Remove(p)
		}
	}
	for _, hex := range digests {
		if !isHexDigest(hex) {
			cleanup()
			return false, fmt.Errorf("peer manifest has non-hex digest %q", hex)
		}
		dest := filepath.Join(blobsDir, "sha256-"+hex)
		if _, statErr := os.Stat(dest); statErr == nil {
			continue // already have this blob
		}
		if err := fetchBlob(ctx, base, hex, dest); err != nil {
			cleanup()
			return false, err
		}
		written = append(written, dest)
	}

	// Manifest last — only after all blobs landed, so a present manifest always
	// implies present blobs (resolveGGUF reads the manifest, then the blob).
	reg, ns, name, tag := parseModelRef(modelName)
	manifestPath := filepath.Join(models, "manifests", reg, ns, name, tag)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		cleanup()
		return false, err
	}
	if err := writeFileAtomic(manifestPath, manifestData); err != nil {
		cleanup()
		return false, err
	}
	return true, nil
}

// peerHasModel asks the peer's /has endpoint whether it holds modelName.
func peerHasModel(ctx context.Context, base, modelName string) (bool, error) {
	data, err := getBytes(ctx, base+"/has?model="+url.QueryEscape(modelName))
	if err != nil {
		return false, err
	}
	var out struct {
		Has bool `json:"has"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return false, err
	}
	return out.Has, nil
}

// getBytes GETs endpoint and returns the body, erroring on non-200.
func getBytes(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// fetchBlob streams /blob?digest=<hex> into dest atomically (temp file + rename).
func fetchBlob(ctx context.Context, base, hex, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/blob?digest="+hex, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blob %s: %s", hex, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "sha256-*.partial")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// writeFileAtomic writes data to dest via a temp file + rename in the same dir.
func writeFileAtomic(dest string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
