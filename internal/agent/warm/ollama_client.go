package warm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ollamaClient is the concrete OllamaControl over the local Ollama
// daemon's HTTP API. Base is typically http://127.0.0.1:11434.
type ollamaClient struct {
	base string
	hc   *http.Client
}

// NewOllamaClient builds an OllamaControl targeting baseURL.
func NewOllamaClient(baseURL string) OllamaControl {
	return &ollamaClient{
		base: strings.TrimRight(baseURL, "/"),
		hc:   &http.Client{},
	}
}

// EnsureResident pins the model with keep_alive:-1 via a minimal
// /api/generate call. When the model is missing and pull is true it is
// pulled first, then the pin is retried.
func (c *ollamaClient) EnsureResident(ctx context.Context, model string, pull bool) error {
	err := c.pin(ctx, model)
	if err == nil {
		return nil
	}
	if !pull || !isMissingModel(err) {
		return err
	}
	if perr := c.pull(ctx, model); perr != nil {
		return fmt.Errorf("pull %q: %w", model, perr)
	}
	return c.pin(ctx, model)
}

// pin sends a minimal generate request that loads the model and holds it
// forever (keep_alive:-1). An empty prompt just triggers the load.
func (c *ollamaClient) pin(ctx context.Context, model string) error {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	body := map[string]any{"model": model, "keep_alive": -1, "stream": false}
	return c.postExpectOK(pctx, "/api/generate", body)
}

// Release unloads the model by requesting keep_alive:0.
func (c *ollamaClient) Release(ctx context.Context, model string) error {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body := map[string]any{"model": model, "keep_alive": 0, "stream": false}
	return c.postExpectOK(rctx, "/api/generate", body)
}

// pull downloads a model (blocking, non-streaming).
func (c *ollamaClient) pull(ctx context.Context, model string) error {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	body := map[string]any{"name": model, "stream": false}
	return c.postExpectOK(pctx, "/api/pull", body)
}

// ModelSizeBytes reads the model's on-disk size from /api/tags.
func (c *ollamaClient) ModelSizeBytes(ctx context.Context, model string) (uint64, error) {
	tags, err := c.tags(ctx)
	if err != nil {
		return 0, err
	}
	want := normalizeModel(model)
	for _, m := range tags {
		if normalizeModel(m.Name) == want || normalizeModel(m.Model) == want {
			if m.Size < 0 {
				return 0, nil
			}
			return uint64(m.Size), nil
		}
	}
	return 0, nil
}

// OnDisk reports whether the model appears in /api/tags.
func (c *ollamaClient) OnDisk(ctx context.Context, model string) bool {
	tags, err := c.tags(ctx)
	if err != nil {
		return false
	}
	want := normalizeModel(model)
	for _, m := range tags {
		if normalizeModel(m.Name) == want || normalizeModel(m.Model) == want {
			return true
		}
	}
	return false
}

// LoadedModels reads currently-resident model names from /api/ps.
func (c *ollamaClient) LoadedModels(ctx context.Context) ([]string, error) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, c.base+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/ps: HTTP %d", resp.StatusCode)
	}
	var pr struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pr); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pr.Models))
	for _, m := range pr.Models {
		n := m.Name
		if n == "" {
			n = m.Model
		}
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

type tagModel struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	Size  int64  `json:"size"`
}

func (c *ollamaClient) tags(ctx context.Context) ([]tagModel, error) {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, c.base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/tags: HTTP %d", resp.StatusCode)
	}
	var tr struct {
		Models []tagModel `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return nil, err
	}
	return tr.Models, nil
}

// postExpectOK POSTs a JSON body and treats any 2xx as success. A
// non-2xx returns an error carrying the body so isMissingModel can
// classify a "model not found" 404.
func (c *ollamaClient) postExpectOK(ctx context.Context, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.body) }

// isMissingModel classifies an Ollama error as "the model isn't
// downloaded yet" (a 404, or a body mentioning it). Ollama phrases this
// as "model '<name>' not found, try pulling it first".
func isMissingModel(err error) bool {
	var he *httpError
	if !asHTTPError(err, &he) {
		return false
	}
	if he.status == http.StatusNotFound {
		return true
	}
	lb := strings.ToLower(he.body)
	return strings.Contains(lb, "not found") || strings.Contains(lb, "try pulling")
}

func asHTTPError(err error, target **httpError) bool {
	for err != nil {
		if he, ok := err.(*httpError); ok {
			*target = he
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// normalizeModel strips a trailing ":latest" so "llama3.2" and
// "llama3.2:latest" compare equal.
func normalizeModel(name string) string {
	name = strings.TrimSpace(name)
	return strings.TrimSuffix(name, ":latest")
}
