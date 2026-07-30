package recipebuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultInterval = 5 * time.Minute
	fetchTimeout    = 30 * time.Second
	recipesPath     = "/api/v1/recipes"
)

// Config configures a Builder. CloudboxBase, AccessToken and RuntimeContainer
// are required; the rest default.
type Config struct {
	// CloudboxBase is the cloudbox origin (scheme+host); recipesPath is appended.
	CloudboxBase string
	// AccessToken is the per-outpost bearer; must carry recipes:read.
	AccessToken string
	// RuntimeContainer is the <AgentName>-runtime container whose k3s containerd
	// receives the built images.
	RuntimeContainer string
	// WorkDir holds cloned git build contexts. Empty → <TempDir>/outpost-recipes.
	WorkDir string
	// Platform is the build target, e.g. "linux/arm64". Empty → linux/<GOARCH>
	// (native — the whole point: no cross-arch, no manifest lists).
	Platform string
	// Interval between polls. <=0 → defaultInterval.
	Interval time.Duration
	// HTTPClient for the recipe fetch. nil → http.DefaultClient.
	HTTPClient *http.Client
	// Runner performs clone/build/load. nil → a bashy-shelling execRunner
	// (BashyBin must then be set).
	Runner Runner
	// BashyBin is the resolved `bashy` executable for the default execRunner.
	BashyBin string
}

// Builder polls the cloudbox recipe index and builds+loads each recipe locally.
type Builder struct {
	cfg      Config
	runner   Runner
	platform string
	workDir  string
	// built maps recipe name → the context_sha256 last successfully built, so an
	// unchanged recipe is a no-op and only a changed one rebuilds.
	built map[string]string
}

// New constructs a Builder, applying defaults.
func New(cfg Config) *Builder {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	platform := cfg.Platform
	if platform == "" {
		platform = "linux/" + runtime.GOARCH
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "outpost-recipes")
	}
	var r Runner = cfg.Runner
	if r == nil {
		r = execRunner{bashyBin: cfg.BashyBin}
	}
	return &Builder{cfg: cfg, runner: r, platform: platform, workDir: workDir, built: map[string]string{}}
}

// Run blocks until ctx is canceled, polling on the configured cadence. A
// misconfigured Builder (unpaired, no runtime container) logs once and blocks —
// it never errors the errgroup it runs under.
func (b *Builder) Run(ctx context.Context) error {
	if strings.TrimSpace(b.cfg.CloudboxBase) == "" || strings.TrimSpace(b.cfg.AccessToken) == "" ||
		strings.TrimSpace(b.cfg.RuntimeContainer) == "" {
		slog.Info("recipebuilder: not configured (unpaired or no runtime); disabled")
		<-ctx.Done()
		return nil
	}
	slog.Info("recipebuilder: starting", "platform", b.platform, "runtime", b.cfg.RuntimeContainer,
		"interval", b.cfg.Interval.String())
	t := time.NewTicker(b.cfg.Interval)
	defer t.Stop()
	b.tick(ctx) // build on startup, then on the cadence
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			b.tick(ctx)
		}
	}
}

// tick fetches the recipe set and builds every changed one. All failures are
// logged, not fatal — the next tick retries.
func (b *Builder) tick(ctx context.Context) {
	recipes, err := b.fetchRecipes(ctx)
	if err != nil {
		slog.Warn("recipebuilder: fetch recipes failed", "err", err)
		return
	}
	for _, r := range recipes {
		if err := r.validate(); err != nil {
			slog.Warn("recipebuilder: skipping invalid recipe", "name", r.Name, "err", err)
			continue
		}
		if b.built[r.Name] == r.ContextSha256 && r.ContextSha256 != "" {
			present, err := b.runner.ImagePresent(ctx, r.ref(), b.cfg.RuntimeContainer)
			if err != nil {
				slog.Warn("recipebuilder: image presence check failed", "recipe", r.Name, "err", err)
				continue
			}
			if present {
				continue // exact source is still loaded in the current runtime
			}
		}
		if err := b.buildOne(ctx, r); err != nil {
			slog.Warn("recipebuilder: build+load failed", "recipe", r.Name, "err", err)
			continue
		}
		b.built[r.Name] = r.ContextSha256
		slog.Info("recipebuilder: built+loaded", "recipe", r.Name, "ref", r.ref())
	}
}

// buildOne resolves the recipe's context, builds it natively, and loads it into
// the node's containerd.
func (b *Builder) buildOne(ctx context.Context, r Recipe) error {
	var ctxRoot string
	switch r.ContextType {
	case "local":
		ctxRoot = r.ContextPath
	case "git":
		dest := filepath.Join(b.workDir, r.Name)
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("clean workdir: %w", err)
		}
		if err := b.runner.Clone(ctx, r.ContextRepo, r.ContextRef, dest); err != nil {
			return err
		}
		ctxRoot = dest
	case "inline_tar":
		raw, err := decodeInlineArchive(r.ContextArchive, r.ContextSha256)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(b.workDir, 0o700); err != nil {
			return fmt.Errorf("create workdir: %w", err)
		}
		dest := filepath.Join(b.workDir, r.Name)
		tmp, err := os.MkdirTemp(b.workDir, "."+r.Name+"-")
		if err != nil {
			return fmt.Errorf("create context tempdir: %w", err)
		}
		defer os.RemoveAll(tmp)
		if err := extractInlineArchive(raw, tmp); err != nil {
			return err
		}
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("clean workdir: %w", err)
		}
		if err := os.Rename(tmp, dest); err != nil {
			return fmt.Errorf("install context: %w", err)
		}
		ctxRoot = dest
	default:
		return fmt.Errorf("unsupported context_type %q", r.ContextType)
	}
	if r.ContextSubdir != "" {
		ctxRoot = filepath.Join(ctxRoot, r.ContextSubdir)
	}
	dockerfile := filepath.Join(ctxRoot, r.Dockerfile)

	if err := b.runner.Build(ctx, b.platform, dockerfile, r.ref(), ctxRoot); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if err := b.runner.Load(ctx, r.ref(), b.cfg.RuntimeContainer); err != nil {
		return fmt.Errorf("load: %w", err)
	}
	present, err := b.runner.ImagePresent(ctx, r.ref(), b.cfg.RuntimeContainer)
	if err != nil {
		return fmt.Errorf("verify loaded image: %w", err)
	}
	if !present {
		return fmt.Errorf("verify loaded image: %s is absent from runtime %s", r.ref(), b.cfg.RuntimeContainer)
	}
	return nil
}

// recipesResponse is the /api/v1/recipes list shape (the V1AssetResponse the
// cloudbox handler returns under "recipes").
type recipesResponse struct {
	Recipes []struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	} `json:"recipes"`
}

// fetchRecipes GETs the caller-visible recipe set and parses each Content YAML.
func (b *Builder) fetchRecipes(ctx context.Context) ([]Recipe, error) {
	url := strings.TrimRight(b.cfg.CloudboxBase, "/") + recipesPath
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.AccessToken)

	client := b.cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudbox %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out recipesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode recipes: %w", err)
	}
	recipes := make([]Recipe, 0, len(out.Recipes))
	for _, r := range out.Recipes {
		rec := parseRecipe(r.Content)
		if rec.Name == "" {
			rec.Name = r.Name // fall back to the asset name
		}
		recipes = append(recipes, rec)
	}
	return recipes, nil
}
