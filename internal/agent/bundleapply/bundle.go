package bundleapply

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Bundle is an ordered set of Kubernetes objects decoded from a manifest
// path. Objects are already sorted into apply order (see applyRank).
type Bundle struct {
	// Objects in apply order: cluster-scoped prerequisites (Namespaces,
	// CRDs, RBAC) first, then namespaced workloads.
	Objects []*unstructured.Unstructured
}

// LoadBundle reads a bundle from a path — either a single manifest file
// or a directory of *.yaml / *.yml / *.json files (recursively) — splits
// multi-document YAML, decodes each document into an unstructured object,
// and returns them in apply order.
//
// An empty result is ErrEmptyBundle, never a silent success: pointing at
// a path that yields nothing is a failure per the evidence invariant.
func LoadBundle(path string) (*Bundle, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("bundleapply: empty bundle path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("bundleapply: stat bundle path %q: %w", path, err)
	}

	var files []string
	if info.IsDir() {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".yaml", ".yml", ".json":
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("bundleapply: walk bundle dir %q: %w", path, err)
		}
		// Deterministic order across platforms/filesystems before the
		// stable apply-rank sort runs.
		sort.Strings(files)
		if len(files) == 0 {
			return nil, fmt.Errorf("bundleapply: no .yaml/.yml/.json files under %q: %w", path, ErrEmptyBundle)
		}
	} else {
		files = []string{path}
	}

	var objs []*unstructured.Unstructured
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("bundleapply: read %q: %w", f, err)
		}
		fileObjs, err := decodeManifests(raw)
		if err != nil {
			return nil, fmt.Errorf("bundleapply: decode %q: %w", f, err)
		}
		objs = append(objs, fileObjs...)
	}

	if len(objs) == 0 {
		return nil, ErrEmptyBundle
	}

	// Installer contract (lifecycle.go): enforce it at load time so
	// apply, status, and uninstall all refuse a contract-violating
	// manifest identically, before any cluster is touched.
	if err := validateLifecycle(objs); err != nil {
		return nil, err
	}

	sortByApplyRank(objs)
	return &Bundle{Objects: objs}, nil
}

// decodeManifests splits a byte stream into individual YAML/JSON
// documents and decodes each into an unstructured object. Empty documents
// (blank, or comment-only) are skipped; a document that is present but
// malformed — or missing apiVersion/kind — is a hard error, never
// silently dropped (an unparseable signal must fail, not vanish).
func decodeManifests(raw []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode document: %w", err)
		}
		if len(m) == 0 {
			// Blank or comment-only document between `---` separators.
			continue
		}
		obj := &unstructured.Unstructured{Object: m}
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" {
			return nil, fmt.Errorf("document missing apiVersion or kind: %v", m)
		}
		if strings.TrimSpace(obj.GetName()) == "" {
			return nil, fmt.Errorf("%s document missing metadata.name", obj.GetKind())
		}
		out = append(out, obj)
	}
	return out, nil
}
