// Package recipebuilder is the per-node half of the DKS "recipes, not blobs"
// image-distribution model (dhnt/docs/dks-image-recipe-distribution-design.md).
//
// It polls cloudbox's recipe index (GET /api/v1/recipes, scope recipes:read),
// and for each recipe this node doesn't already have built, resolves the build
// context, builds the image NATIVELY with `bashy podman`, and imports it into
// this node's k3s containerd. No image blob is ever transferred — each node
// reproduces the image from the recipe. It is the automated form of
// script/dks-image/build-load.sh.
package recipebuilder

import (
	"strings"
)

// Recipe is the parsed build recipe (the YAML carried in a cloudbox Recipe
// asset's Content). The wire/authoring form is the flat YAML in
// script/dks-image/recipes/*.yaml; we hand-parse the top-level keys we need so
// the lean outpost daemon adds no YAML dependency.
type Recipe struct {
	Name          string
	Tag           string
	LocalRef      string   // what the node loads it as (localhost/cluster/<n>)
	ContextType   string   // git | local
	ContextRepo   string   // git remote (context_type=git)
	ContextRef    string   // pinned commit/ref (context_type=git)
	ContextSubdir string   // build context root within the checkout
	ContextPath   string   // local path (context_type=local; same-host only)
	Dockerfile    string   // relative to the context root
	BaseImages    []string // declared bases (mirrored to GHCR out of band)
	ContextSha256 string   // provenance of source (verified, not output digest)
}

// parseRecipe decodes the flat top-level keys from a recipe YAML body. Unknown
// lines and comments are ignored; quoted scalars are unquoted. Absent optional
// fields stay zero.
func parseRecipe(body string) Recipe {
	var r Recipe
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		// only top-level keys (no leading indentation)
		if line != t {
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = unquote(strings.TrimSpace(v))
		switch strings.TrimSpace(k) {
		case "name":
			r.Name = v
		case "tag":
			r.Tag = v
		case "local_ref":
			r.LocalRef = v
		case "context_type":
			r.ContextType = v
		case "context_repo":
			r.ContextRepo = v
		case "context_ref":
			r.ContextRef = v
		case "context_subdir":
			r.ContextSubdir = v
		case "context_path":
			r.ContextPath = v
		case "dockerfile":
			r.Dockerfile = v
		case "base_images":
			r.BaseImages = strings.Fields(v)
		case "context_sha256":
			r.ContextSha256 = v
		}
	}
	return r
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// ref returns "<local_ref>:<tag>", the tag the node loads and pods reference.
func (r Recipe) ref() string {
	tag := r.Tag
	if tag == "" {
		tag = "latest"
	}
	return r.LocalRef + ":" + tag
}

// valid reports whether the recipe has the minimum to build.
func (r Recipe) valid() bool {
	return r.Name != "" && r.LocalRef != "" && r.Dockerfile != "" &&
		(r.ContextType == "local" && r.ContextPath != "" ||
			r.ContextType == "git" && r.ContextRepo != "")
}
