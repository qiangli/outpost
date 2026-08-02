package peerimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent/recipebuilder"
)

// nameRE bounds a recipe name to something that is safe as a single path
// element AND as a URL path segment, so neither the store nor the index server
// can be walked out of.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidRecipeDigest reports whether s is a well-formed "sha256:<64 hex>".
func ValidRecipeDigest(s string) bool {
	const p = "sha256:"
	if !strings.HasPrefix(s, p) || len(s) != len(p)+64 {
		return false
	}
	_, err := hex.DecodeString(s[len(p):])
	return err == nil
}

// Publication is one published recipe as the index reports it. It carries no
// build context — a peer fetches the recipe document itself to get that.
type Publication struct {
	Name        string    `json:"name"`
	Ref         string    `json:"ref"`
	Digest      string    `json:"recipe_digest"`
	PublishedAt time.Time `json:"published_at"`
}

// Store is the node-local recipe + provenance store. Recipes are what peers
// fetch; provenance is private to this node (it is what its own live digest is
// correlated against) and is never served.
type Store struct {
	dir string
	mu  sync.Mutex
	now func() time.Time
}

// NewStore opens (creating if needed) a store rooted at dir. Mode 0700: a
// recipe can carry an inline build context, which is source code.
func NewStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("store dir is required")
	}
	for _, sub := range []string{"recipes", "provenance"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create store: %w", err)
		}
	}
	return &Store{dir: dir, now: time.Now}, nil
}

func (s *Store) recipePath(name string) string {
	return filepath.Join(s.dir, "recipes", name+".yaml")
}

// provenancePath keys by the sha256 of the ref rather than the ref itself: a
// ref contains '/' and ':' and is not a legal path element on every platform.
func (s *Store) provenancePath(ref string) string {
	return filepath.Join(s.dir, "provenance", hashKey(ref)+".json")
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Publish validates and stores a recipe document, returning its identity.
// Republishing the same name overwrites — the digest tells peers whether
// anything actually changed.
//
// Side-effect class: Live. Nothing here is read at boot, so no restart.
func (s *Store) Publish(name, body string) (Publication, error) {
	rec, err := recipebuilder.ParseRecipe(body)
	if err != nil {
		return Publication{}, fmt.Errorf("invalid recipe: %w", err)
	}
	if strings.TrimSpace(name) == "" {
		name = rec.Name
	}
	if !nameRE.MatchString(name) {
		return Publication{}, fmt.Errorf("invalid recipe name")
	}
	if rec.Name != name {
		return Publication{}, fmt.Errorf("recipe name %q does not match the published name %q", rec.Name, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeFileAtomic(s.recipePath(name), []byte(body), 0o600); err != nil {
		return Publication{}, err
	}
	return Publication{
		Name: name, Ref: rec.Ref(), Digest: rec.Digest(), PublishedAt: s.now().UTC(),
	}, nil
}

// Recipe returns the stored recipe document and its parsed form.
// A missing recipe is ErrNoRecipe — never an empty Recipe with a nil error.
func (s *Store) Recipe(name string) (string, recipebuilder.Recipe, error) {
	if !nameRE.MatchString(name) {
		return "", recipebuilder.Recipe{}, fmt.Errorf("invalid recipe name")
	}
	body, err := os.ReadFile(s.recipePath(name))
	if os.IsNotExist(err) {
		return "", recipebuilder.Recipe{}, fmt.Errorf("%w: %s", ErrNoRecipe, name)
	}
	if err != nil {
		return "", recipebuilder.Recipe{}, err
	}
	rec, perr := recipebuilder.ParseRecipe(string(body))
	if perr != nil {
		return "", recipebuilder.Recipe{}, fmt.Errorf("stored recipe %s is invalid: %w", name, perr)
	}
	return string(body), rec, nil
}

// List returns every published recipe, sorted by name. An unreadable or
// invalid entry is an error: a truncated listing that silently drops a recipe
// would let "the peer doesn't have it" mean "we failed to read it".
func (s *Store) List() ([]Publication, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "recipes"))
	if err != nil {
		return nil, err
	}
	out := make([]Publication, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		_, rec, rerr := s.Recipe(name)
		if rerr != nil {
			return nil, rerr
		}
		info, ierr := e.Info()
		var at time.Time
		if ierr == nil {
			at = info.ModTime().UTC()
		}
		out = append(out, Publication{Name: name, Ref: rec.Ref(), Digest: rec.Digest(), PublishedAt: at})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// PutProvenance records what this node built. Called only after the content
// digest has actually been read back out of containerd.
func (s *Store) PutProvenance(p Provenance) error {
	if !ValidContentDigest(p.ContentDigest) {
		return fmt.Errorf("refusing to record provenance without a valid content digest")
	}
	if !ValidRecipeDigest(p.RecipeDigest) {
		return fmt.Errorf("refusing to record provenance without a valid recipe digest")
	}
	if strings.TrimSpace(p.Node) == "" || strings.TrimSpace(p.Ref) == "" {
		return fmt.Errorf("provenance must name its node and ref")
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.provenancePath(p.Ref), blob, 0o600)
}

// Provenance returns this node's build record for ref. The bool is false when
// there is no record — which callers must treat as "cannot attribute these
// bytes", not as "fine".
func (s *Store) Provenance(ref string) (Provenance, bool, error) {
	blob, err := os.ReadFile(s.provenancePath(ref))
	if os.IsNotExist(err) {
		return Provenance{}, false, nil
	}
	if err != nil {
		return Provenance{}, false, err
	}
	var p Provenance
	if uerr := json.Unmarshal(blob, &p); uerr != nil {
		return Provenance{}, false, fmt.Errorf("provenance for %s is unreadable: %w", ref, uerr)
	}
	return p, true, nil
}

// writeFileAtomic writes via a temp file + rename so a crashed write never
// leaves a half-parsed recipe that would build the wrong thing.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
