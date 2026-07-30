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
	"fmt"
	"regexp"
	"strings"
)

var (
	recipeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	imageRefRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*$`)
	imageTagRE   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// Recipe is the parsed build recipe (the YAML carried in a cloudbox Recipe
// asset's Content). The wire/authoring form is the flat YAML in
// script/dks-image/recipes/*.yaml; we hand-parse the top-level keys we need so
// the lean outpost daemon adds no YAML dependency.
type Recipe struct {
	Name           string
	Tag            string
	LocalRef       string   // what the node loads it as (localhost/cluster/<n>)
	ContextType    string   // git | local | inline_tar
	ContextRepo    string   // git remote (context_type=git)
	ContextRef     string   // pinned commit/ref (context_type=git)
	ContextSubdir  string   // build context root within the checkout
	ContextPath    string   // local path (context_type=local; same-host only)
	ContextArchive string   // base64 tar or tar.gz bytes (context_type=inline_tar)
	Dockerfile     string   // relative to the context root
	BaseImages     []string // declared bases (mirrored to GHCR out of band)
	ContextSha256  string   // provenance of source (verified, not output digest)
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
		case "context_archive_base64":
			r.ContextArchive = v
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
	return r.validate() == nil
}

// validate rejects recipes whose names or relative paths could escape the
// builder's work directory. inline_tar additionally requires an authenticated
// byte-for-byte digest before any archive parsing or filesystem writes.
func (r Recipe) validate() error {
	if !recipeNameRE.MatchString(r.Name) {
		return fmt.Errorf("invalid recipe name")
	}
	if !imageRefRE.MatchString(r.LocalRef) {
		return fmt.Errorf("invalid local_ref")
	}
	if r.Tag != "" && !imageTagRE.MatchString(r.Tag) {
		return fmt.Errorf("invalid tag")
	}
	if !safeRelativePath(r.Dockerfile) {
		return fmt.Errorf("dockerfile must be a safe relative path")
	}
	if r.ContextSubdir != "" && !safeRelativePath(r.ContextSubdir) {
		return fmt.Errorf("context_subdir must be a safe relative path")
	}
	switch r.ContextType {
	case "local":
		if strings.TrimSpace(r.ContextPath) == "" {
			return fmt.Errorf("context_path is required")
		}
	case "git":
		if strings.TrimSpace(r.ContextRepo) == "" || strings.HasPrefix(r.ContextRepo, "-") {
			return fmt.Errorf("invalid context_repo")
		}
		if strings.HasPrefix(r.ContextRef, "-") {
			return fmt.Errorf("invalid context_ref")
		}
	case "inline_tar":
		if strings.TrimSpace(r.ContextArchive) == "" {
			return fmt.Errorf("context_archive_base64 is required")
		}
		if !validSHA256(r.ContextSha256) {
			return fmt.Errorf("context_sha256 must be 64 hexadecimal characters")
		}
	default:
		return fmt.Errorf("unsupported context_type %q", r.ContextType)
	}
	return nil
}
