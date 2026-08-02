package bundleapply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memcache "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager is the server-side-apply field manager this package owns.
// A stable manager string is what makes repeated applies converge instead
// of fighting each other over field ownership.
const FieldManager = "outpost-bundleapply"

// ResourceClient is the minimal Kubernetes surface bundleapply needs. It
// is an interface so the apply/wait logic can be driven by a deterministic
// in-memory fake in tests — no apiserver, no kubeconfig — while production
// uses the dynamic-client implementation below.
type ResourceClient interface {
	// Apply performs an idempotent server-side apply of obj (create if
	// absent, reconcile if present) and returns the applied object.
	Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// Get fetches the live object matching obj's GVK / namespace / name.
	// A missing object is reported as an error wrapping ErrNotFound so
	// the apply loop can tell "does not exist yet" (this run will create
	// it) from every other failure, which stays hard.
	Get(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// Delete removes the live object matching obj's GVK / namespace /
	// name. Deleting an object that is already gone is a success — the
	// rollback path must converge, not fight over who removed it.
	Delete(ctx context.Context, obj *unstructured.Unstructured) error
	// ServesGVK reports whether the apiserver's discovery currently
	// serves the given GroupVersionKind. Used to gate custom resources
	// behind their CRD actually being discoverable — an Established CRD
	// whose type has not reached the discovery cache still 404s.
	ServesGVK(ctx context.Context, gvk schema.GroupVersionKind) (bool, error)
}

// dynamicClient is the production ResourceClient: a client-go dynamic
// client plus a discovery-backed RESTMapper that resolves each object's
// GroupVersionKind to the GroupVersionResource the dynamic client needs.
type dynamicClient struct {
	dyn    dynamic.Interface
	disco  discovery.DiscoveryInterface
	cache  discovery.CachedDiscoveryInterface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

// ExpandUser expands a leading ~ or ~/ in a path to the current user's
// home directory. It exists so callers can accept the conventional
// "~/.kube/..." kubeconfig paths without a shell doing the expansion.
func ExpandUser(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("bundleapply: resolve home dir for %q: %w", path, err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// NewDynamicClient builds a ResourceClient from a kubeconfig PATH. The
// path is required and must exist — there is NO fallback to an in-cluster
// config or a default cluster (a missing kubeconfig fails loudly, per the
// no-silent-fallback rule).
//
// The venue guard is enforced HERE, in the Go API: the path is
// canonicalized (tilde expansion, absolutization, symlink resolution) and
// refused when it lands on the cloudbox kubeconfig — so a caller using
// this package directly cannot bypass the guard the operator script also
// applies. A path that cannot be canonicalized is an error.
//
// Nothing on this path touches cloudbox: it reads the kubeconfig file and
// dials the apiserver named in it, nothing more.
func NewDynamicClient(kubeconfigPath string) (ResourceClient, error) {
	kubeconfigPath = strings.TrimSpace(kubeconfigPath)
	if kubeconfigPath == "" {
		return nil, ErrNoKubeconfig
	}
	expanded, err := ExpandUser(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(expanded); err != nil {
		return nil, fmt.Errorf("bundleapply: kubeconfig %q: %w: %v", expanded, ErrNoKubeconfig, err)
	}
	canonical, err := ResolveVenue(kubeconfigPath)
	if err != nil {
		return nil, err
	}

	restCfg, err := clientcmd.BuildConfigFromFlags("", canonical)
	if err != nil {
		return nil, fmt.Errorf("bundleapply: load kubeconfig %q: %w", canonical, err)
	}

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("bundleapply: build dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("bundleapply: build discovery client: %w", err)
	}
	cache := memcache.NewMemCacheClient(disco)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cache)
	return &dynamicClient{dyn: dyn, disco: disco, cache: cache, mapper: mapper}, nil
}

// resourceFor resolves an object's GVK to a namespaced or cluster-scoped
// dynamic resource interface.
func (c *dynamicClient) resourceFor(obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("bundleapply: resolve %s: %w", gvk.String(), err)
	}
	if mapping.Scope.Name() == "namespace" {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		return c.dyn.Resource(mapping.Resource).Namespace(ns), nil
	}
	return c.dyn.Resource(mapping.Resource), nil
}

func (c *dynamicClient) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := c.resourceFor(obj)
	if err != nil {
		return nil, err
	}
	applied, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: FieldManager,
		Force:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("bundleapply: apply %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}
	return applied, nil
}

func (c *dynamicClient) Get(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := c.resourceFor(obj)
	if err != nil {
		return nil, err
	}
	live, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("bundleapply: get %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), ErrNotFound)
		}
		return nil, fmt.Errorf("bundleapply: get %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}
	return live, nil
}

func (c *dynamicClient) Delete(ctx context.Context, obj *unstructured.Unstructured) error {
	ri, err := c.resourceFor(obj)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("bundleapply: delete %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// ServesGVK asks live discovery whether gvk is currently served. The
// discovery cache is invalidated first — the entire point of the call is
// to observe a CRD-registered type APPEARING, which a stale cache would
// hide (and a stale mapper would turn a hit into a NoMatch on the next
// apply).
func (c *dynamicClient) ServesGVK(_ context.Context, gvk schema.GroupVersionKind) (bool, error) {
	c.cache.Invalidate()
	c.mapper.Reset()
	list, err := c.disco.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("bundleapply: discovery for %s: %w", gvk.GroupVersion(), err)
	}
	for _, r := range list.APIResources {
		if r.Kind == gvk.Kind {
			return true, nil
		}
	}
	return false, nil
}
