package bundleapply

import "errors"

var (
	// ErrEmptyBundle is returned when a bundle path yields zero
	// applicable objects. An empty bundle is a failure, never a silent
	// success (evidence invariant): if the operator pointed us at a path,
	// something was expected to be there.
	ErrEmptyBundle = errors.New("bundleapply: bundle contains no Kubernetes objects")

	// ErrReadinessTimeout is returned by WaitForReady when at least one
	// object did not report Ready before the deadline. The caller must
	// exit non-zero on this error.
	ErrReadinessTimeout = errors.New("bundleapply: timed out waiting for objects to become Ready")

	// ErrNoKubeconfig is returned when the kubeconfig path is empty or the
	// file does not exist. There is no fallback to a default cluster — a
	// missing kubeconfig fails loudly.
	ErrNoKubeconfig = errors.New("bundleapply: kubeconfig path is empty or missing")

	// ErrCloudboxVenue is returned by the venue guard when the kubeconfig
	// path — after tilde expansion, absolutization, and symlink
	// resolution — is the CLOUDBOX kubeconfig. A peer bundle apply must
	// never land on the cloudbox plane, however the path was spelled.
	ErrCloudboxVenue = errors.New("bundleapply: refusing to apply a peer bundle against the cloudbox kubeconfig — the peer and cloudbox planes are never conflated")

	// ErrVenueUnresolvable is returned when the kubeconfig path cannot be
	// canonicalized (missing file, dangling symlink, unresolvable
	// component). An unresolvable venue FAILS — it is never "probably
	// fine".
	ErrVenueUnresolvable = errors.New("bundleapply: kubeconfig path cannot be canonicalized")

	// ErrNotFound marks a Get whose object does not exist on the cluster.
	// The apply loop uses it to decide whether THIS run created an object
	// (and may therefore roll it back) — every other Get failure stays a
	// hard error.
	ErrNotFound = errors.New("bundleapply: object not found")

	// ErrCRDNotReady is returned when a CustomResourceDefinition shipped
	// in the bundle did not reach the Established condition — or its
	// served types did not appear in apiserver discovery — before the
	// bounded wait expired. Applying the dependent custom resources
	// anyway would race the discovery cache (the pass-locally,
	// flake-in-the-field failure class), so this is a hard stop.
	ErrCRDNotReady = errors.New("bundleapply: CustomResourceDefinition not Established / not in discovery before the deadline")

	// ErrZeroDesiredWorkload is returned when a workload declares
	// spec.replicas: 0 and the caller did not opt in via
	// AllowScaleToZero. Zero desired replicas satisfying "all replicas
	// ready" is a readiness verdict resting on the absence of evidence —
	// exactly what this package refuses to green-light.
	ErrZeroDesiredWorkload = errors.New("bundleapply: workload declares zero desired replicas — trivially 'ready' with nothing running; opt in with allow-scale-to-zero if intended")
)
