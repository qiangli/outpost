package admincore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/bundleapply"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// Peer-DKS bundle apply — the four-surface home of what
// script/dks-peer-bundle-apply.sh does: install an app/bundle manifest
// set against a PEER-HOSTED control plane addressed purely by a
// kubeconfig path.
//
// Every surface (MCP tool `outpost_apply_bundle`, CLI `outpost bundle
// apply`, and the standalone script's Go runner) converges on
// Server.BundleApply, so the venue guard, the readiness bar, and the
// rollback accounting cannot drift between them.
//
// NO CLOUDBOX ON THIS PATH: the operation never reads AccessToken, never
// dials /api/v1, never fetches overlay credentials. It reads a kubeconfig
// file and talks to the apiserver named in it.
//
// The persisted key `cluster.bundle_kubeconfig` is the default venue when
// a call doesn't pass one. Side-effect class: LIVE — the key is read on
// each apply; changing it never restarts the daemon.

// BundleApplyParams is one apply request.
type BundleApplyParams struct {
	// Kubeconfig is the venue. Empty falls back to the persisted
	// cluster.bundle_kubeconfig, then to the conventional peer
	// control-plane path (~/.kube/outpost-control-plane/k3s.yaml). It is
	// canonicalized (symlinks resolved) and refused when it lands on the
	// cloudbox kubeconfig — whichever source supplied it.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// Bundle is the manifest file or directory to apply (required).
	Bundle string `json:"bundle"`
	// TimeoutSeconds bounds the readiness wait for the whole bundle.
	// Default 300.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// PollSeconds is the readiness re-check interval. Default 2.
	PollSeconds int `json:"poll_seconds,omitempty"`
	// CRDTimeoutSeconds bounds, per CRD, the Established+discovery wait
	// before dependent custom resources apply. Default 60.
	CRDTimeoutSeconds int `json:"crd_timeout_seconds,omitempty"`
	// AllowScaleToZero is the explicit opt-in for spec.replicas: 0
	// workloads counting as rolled out once drained.
	AllowScaleToZero bool `json:"allow_scale_to_zero,omitempty"`
	// NoRollback leaves objects this run created in place on failure
	// (still reported precisely).
	NoRollback bool `json:"no_rollback,omitempty"`
	// SaveKubeconfig persists a non-empty Kubeconfig as
	// cluster.bundle_kubeconfig after the venue guard accepts it, so
	// later applies can omit the path. Live — no restart.
	SaveKubeconfig bool `json:"save_kubeconfig,omitempty"`
}

// BundleApplyResult reports one apply run. On failure the same accounting
// travels inside the returned error text (the HTTP/MCP layers only carry
// the error), so nothing about a partial apply is ever silent.
type BundleApplyResult struct {
	OK bool `json:"ok"`
	// Kubeconfig is the CANONICAL venue the apply ran against (symlinks
	// resolved) — proof of which plane was touched.
	Kubeconfig string `json:"kubeconfig"`
	Applied    int    `json:"applied"`
	Ready      int    `json:"ready"`
	// Created / RolledBack / CleanupFailed are the transactional
	// accounting: what this run brought into existence, and (on the
	// failure path) what the cleanup removed or could not remove.
	Created         []string `json:"created,omitempty"`
	RolledBack      []string `json:"rolled_back,omitempty"`
	CleanupFailed   []string `json:"cleanup_failed,omitempty"`
	KubeconfigSaved bool     `json:"kubeconfig_saved,omitempty"`
}

// BundleKubeconfigView reports the persisted default venue (no secrets
// involved — a path, not a credential).
type BundleKubeconfigView struct {
	OK bool `json:"ok"`
	// Kubeconfig is the persisted cluster.bundle_kubeconfig ("" when
	// unset — applies then default to the conventional peer path).
	Kubeconfig string `json:"kubeconfig"`
	// Default is the conventional peer path used when nothing is
	// persisted or passed.
	Default string `json:"default"`
}

// BundleKubeconfig returns the persisted default venue.
func (s *Server) BundleKubeconfig() (BundleKubeconfigView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return BundleKubeconfigView{}, err
	}
	out := BundleKubeconfigView{OK: true, Default: bundleapply.PeerControlPlaneKubeconfig}
	if fc.Cluster != nil {
		out.Kubeconfig = fc.Cluster.BundleKubeconfig
	}
	return out, nil
}

// BundleApply loads a bundle, enforces the kubeconfig venue guard, applies
// the bundle in prerequisite order with the CRD Established+discovery
// gate, and waits — bounded — for every workload to be ROLLED OUT (updated
// replicas, StatefulSet revision parity; zero-desired refused without the
// explicit opt-in). On failure it rolls back what the run created (unless
// NoRollback) and reports the accounting in the error.
//
// Side-effect class: Live. No restart is ever scheduled — the operation
// touches the peer cluster, not this daemon's own wiring.
func (s *Server) BundleApply(ctx context.Context, p BundleApplyParams) (BundleApplyResult, error) {
	if strings.TrimSpace(p.Bundle) == "" {
		return BundleApplyResult{}, badRequest("bundle path required (a manifest file or a directory of manifests)")
	}
	if p.SaveKubeconfig && strings.TrimSpace(p.Kubeconfig) == "" {
		return BundleApplyResult{}, badRequest("save_kubeconfig needs an explicit kubeconfig to save")
	}

	// Resolve the venue source under the config mutex: explicit param >
	// persisted cluster.bundle_kubeconfig > conventional peer path.
	s.mu.Lock()
	fc, err := s.loadConfig()
	if err != nil {
		s.mu.Unlock()
		return BundleApplyResult{}, err
	}
	raw := strings.TrimSpace(p.Kubeconfig)
	if raw == "" && fc.Cluster != nil {
		raw = strings.TrimSpace(fc.Cluster.BundleKubeconfig)
	}
	if raw == "" {
		raw = bundleapply.PeerControlPlaneKubeconfig
	}
	s.mu.Unlock()

	// Venue guard — IN THE GO API, before any client is built, so an
	// injected test client can't bypass it either. Canonicalizes the
	// path (tilde, relative, .., symlinks) and refuses the cloudbox
	// kubeconfig; a path that cannot be canonicalized fails.
	canonical, err := bundleapply.ResolveVenue(raw)
	if err != nil {
		if errors.Is(err, bundleapply.ErrCloudboxVenue) {
			return BundleApplyResult{}, badRequest("%s", err.Error())
		}
		return BundleApplyResult{}, badRequest("kubeconfig %q: %s", raw, err.Error())
	}

	b, err := bundleapply.LoadBundle(p.Bundle)
	if err != nil {
		return BundleApplyResult{}, badRequest("%s", err.Error())
	}

	factory := s.deps.BundleApplyClient
	if factory == nil {
		factory = bundleapply.NewDynamicClient
	}
	client, err := factory(canonical)
	if err != nil {
		return BundleApplyResult{}, internalErr("%s", err.Error())
	}

	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if p.TimeoutSeconds <= 0 {
		timeout = 5 * time.Minute
	}
	poll := time.Duration(p.PollSeconds) * time.Second
	crdTimeout := time.Duration(p.CRDTimeoutSeconds) * time.Second

	res, applyErr := bundleapply.ApplyBundle(ctx, b, bundleapply.Options{
		Client:           client,
		Timeout:          timeout,
		PollInterval:     poll,
		CRDWaitTimeout:   crdTimeout,
		AllowScaleToZero: p.AllowScaleToZero,
		DisableRollback:  p.NoRollback,
	})
	out := BundleApplyResult{
		OK:            applyErr == nil,
		Kubeconfig:    canonical,
		Applied:       res.Applied,
		Ready:         res.Ready,
		Created:       res.Created,
		RolledBack:    res.RolledBack,
		CleanupFailed: res.CleanupFailed,
	}
	if applyErr != nil {
		// The error already carries the rollback accounting; surface it
		// as-is (plain error → 500 / MCP IsError) rather than reshaping.
		return out, applyErr
	}

	// Persist the accepted venue only AFTER a successful apply — a saved
	// default that never worked would be a trap for the next run.
	if p.SaveKubeconfig {
		s.mu.Lock()
		defer s.mu.Unlock()
		fc, err := s.loadConfig()
		if err != nil {
			return out, err
		}
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		fc.Cluster.BundleKubeconfig = strings.TrimSpace(p.Kubeconfig)
		if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
			return out, internalErr("bundle applied, but saving bundle_kubeconfig failed: %s", err.Error())
		}
		out.KubeconfigSaved = true
	}
	return out, nil
}
