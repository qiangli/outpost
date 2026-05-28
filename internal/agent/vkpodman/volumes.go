package vkpodman

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// emptyDirHostPath returns the host filesystem path where an
// EmptyDir volume's data is materialized for a given pod. Disk-
// backed EmptyDirs become bind mounts of this directory; tmpfs
// EmptyDirs don't go through here at all.
//
// Layout: <cacheDir>/cluster/emptydir/<podUID>/<volumeName>
// — keyed by pod UID rather than namespace+name so two pods that
// happen to share the same name across separate lifetimes don't
// collide on the same dir. DeletePod removes the per-pod tree.
//
// Pure path-shaping; mkdir is the caller's job so it can decide
// what permission mode to use for the leaf (default 0755 from the
// translator).
func emptyDirHostPath(podUID, volumeName string) (string, error) {
	if podUID == "" {
		return "", errors.New("vkpodman: emptyDir requires pod UID")
	}
	if volumeName == "" {
		return "", errors.New("vkpodman: emptyDir requires volume name")
	}
	base, err := emptyDirRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, podUID, volumeName), nil
}

// emptyDirRoot returns the directory under which per-pod EmptyDir
// trees live. Defaults to <conf.DefaultCacheDir>/cluster/emptydir.
// Wraps the cache-dir lookup so the value is computed once per call
// (rather than threaded through the translator's signature).
func emptyDirRoot() (string, error) {
	cache, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "cluster", "emptydir"), nil
}

// RemoveEmptyDirsForPod removes the per-pod EmptyDir tree at
// DeletePod time. Idempotent — RemoveAll on a missing path is a
// no-op. Called from the Provider's DeletePod path so disk-backed
// EmptyDir lifetime tracks the pod lifetime (which is the k8s
// contract).
func RemoveEmptyDirsForPod(podUID string) error {
	if podUID == "" {
		return nil
	}
	base, err := emptyDirRoot()
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(base, podUID))
}
