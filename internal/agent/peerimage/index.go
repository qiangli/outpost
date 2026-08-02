package peerimage

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/outpost/internal/agent/recipebuilder"
)

// IndexHandler serves this node's published recipes to peers:
//
//	GET /recipes            → the Publication index (JSON)
//	GET /recipes/<name>     → the recipe document (text/yaml)
//
// It is mounted on a LOOPBACK listener and reached by peers only through the
// mesh forwarder under an allowlisted service name — the same boundary every
// other wrapped tool uses. It adds no overlay and widens no allowlist.
//
// It serves recipes only. Provenance is node-private: it is the local anchor a
// node's own live digest is correlated against, and handing it to a peer would
// let a peer's claim be built from someone else's record.
func (s *Service) IndexHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/recipes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pubs, err := s.Publications()
		if err != nil {
			// The error text is not echoed: it can contain local paths, and
			// this host's PTY capture is unredacted.
			http.Error(w, "recipe index unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"recipes": pubs})
	})
	mux.HandleFunc("/recipes/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/recipes/")
		if !nameRE.MatchString(name) {
			http.Error(w, "invalid recipe name", http.StatusBadRequest)
			return
		}
		body, _, err := s.Store.Recipe(name)
		if err != nil {
			http.Error(w, "recipe not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(body))
	})
	return mux
}

// RecipeBuilder adapts recipebuilder's native build+load sequence to the
// Builder interface, so the peer path and the cloudbox-polling path build
// images the exact same way.
type RecipeBuilder struct {
	Runner           recipebuilder.Runner
	WorkDir          string
	Platform         string
	RuntimeContainer string
}

// Materialize parses the recipe document and runs the shared build sequence.
// Empty Platform/WorkDir take recipebuilder's own defaults (native platform,
// a temp workdir) so a caller cannot accidentally build an unlabeled target.
func (b RecipeBuilder) Materialize(ctx context.Context, body string) error {
	rec, err := recipebuilder.ParseRecipe(body)
	if err != nil {
		return err
	}
	platform := b.Platform
	if platform == "" {
		platform = "linux/" + runtime.GOARCH
	}
	workDir := b.WorkDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "outpost-recipes")
	}
	return recipebuilder.Materialize(ctx, b.Runner, workDir, platform, rec, b.RuntimeContainer)
}
