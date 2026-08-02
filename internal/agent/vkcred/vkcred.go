// Package vkcred mints and carries the peer-issued LEAST-PRIVILEGE
// virtual-kubelet credential for a peer-hosted (k3s) control plane.
//
// WHY THIS EXISTS. A peer join hands a worker four values (tunnel endpoint,
// tunnel token, STCP secret, k3s node token) — and none of them can
// authenticate a virtual-kubelet node to the apiserver. The k3s node token is
// a NODE-JOIN credential consumed by `k3s agent --token`; presenting it as a
// bearer token gets a 401. So a fresh `outpost cluster join` with a virtual
// runtime selected used to boot into "no usable credentials" until the
// operator hand-edited agent.json with something — usually the admin
// kubeconfig, which is the one credential that must never travel.
//
// The bundle closes that gap: the HOSTING machine (which holds the plane's
// admin kubeconfig locally) mints a ServiceAccount scoped to exactly the verbs
// vknode's runner uses, plus the namespace allow-list the worker enforces
// fail-closed, and packages token + peer CA + namespaces into one opaque
// string the operator carries to the worker alongside the other three join
// values. The admin kubeconfig itself never leaves the hosting machine.
package vkcred

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Names of the objects Mint provisions on the hosted plane. One identity per
// plane, shared by every joined vk worker: the credential authorizes a ROLE
// (virtual-kubelet on this plane), not a machine, and per-worker identities
// would multiply secrets without narrowing any verb.
const (
	// SystemNamespace holds the ServiceAccount + token secret.
	SystemNamespace = "outpost-system"
	// ServiceAccountName is the vk identity workers authenticate as.
	ServiceAccountName = "outpost-vk"
	// ClusterRoleName carries the minimum verb set vknode's runner uses.
	ClusterRoleName = "outpost-vk-node"
	// TokenSecretName is the kubernetes.io/service-account-token secret the
	// token controller populates; its token + ca.crt are what the bundle
	// carries.
	TokenSecretName = "outpost-vk-token"
)

// bundlePrefix versions the wire form. A new incompatible shape gets a new
// prefix rather than a field the old decoder ignores.
const bundlePrefix = "outpost-vk1."

// Bundle is the credential set a peer-joined virtual-kubelet worker needs and
// a fresh join cannot otherwise obtain: the peer plane's CA identity, a
// least-privilege bearer token, and the fail-closed namespace allow-list.
//
// It maps 1:1 onto the worker's persisted fields — cluster.ca, cluster.token,
// cluster.allowed_namespaces — which is what makes applying it a plain
// config write with no new runtime path.
type Bundle struct {
	// CA is the peer plane's CA certificate (PEM) — the identity the worker
	// pins when dialing its loopback apiserver visitor.
	CA []byte `json:"ca"`
	// Token is the ServiceAccount bearer token, authorized for exactly the
	// ClusterRole verbs below. NOT the k3s node token, NOT an admin credential.
	Token string `json:"token"`
	// Namespaces is the allow-list the worker enforces fail-closed: a pod in
	// any other namespace is refused. Never empty in a valid bundle — an empty
	// policy denies everything, which is not a credential worth carrying.
	Namespaces []string `json:"namespaces"`
}

// Validate rejects a bundle that could not produce a working vk node: the
// worker pins the CA (a peer k3s apiserver is self-signed, so no CA means TLS
// failure), authenticates with the token, and admits pods only from the
// listed namespaces (empty would deny every pod).
func (b Bundle) Validate() error {
	if len(b.CA) == 0 {
		return errors.New("vkcred: bundle has no CA — the worker pins the peer plane's identity with it")
	}
	if strings.TrimSpace(b.Token) == "" {
		return errors.New("vkcred: bundle has no token")
	}
	if len(b.Namespaces) == 0 {
		return errors.New("vkcred: bundle allows no namespaces — the policy is fail-closed, so this would deny every pod")
	}
	for _, ns := range b.Namespaces {
		if err := validateNamespaceName(ns); err != nil {
			return err
		}
	}
	return nil
}

// Encode packs the bundle into the one-line opaque form operators carry
// between machines: a versioned prefix plus base64url(JSON).
func (b Bundle) Encode() (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("vkcred: encode: %w", err)
	}
	return bundlePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode parses and validates an encoded bundle. Errors name what is wrong
// with the VALUE (truncated paste, wrong string entirely) because the operator
// hand-carries it — a bare "invalid input" would not say which of the four
// join values to re-copy.
func Decode(s string) (Bundle, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Bundle{}, errors.New("vkcred: empty bundle")
	}
	if !strings.HasPrefix(s, bundlePrefix) {
		return Bundle{}, fmt.Errorf(
			"vkcred: not a vk credential bundle (expected the %q prefix) — mint one on the hosting machine with `outpost cluster control-plane vk-credential`",
			bundlePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, bundlePrefix))
	if err != nil {
		return Bundle{}, fmt.Errorf("vkcred: malformed bundle (truncated paste?): %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return Bundle{}, fmt.Errorf("vkcred: malformed bundle: %w", err)
	}
	if err := b.Validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// dns1123Label is the namespace-name grammar (RFC 1123 label, ≤63 chars).
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func validateNamespaceName(ns string) error {
	if !dns1123Label.MatchString(ns) {
		return fmt.Errorf("vkcred: invalid namespace name %q (must be a lowercase RFC 1123 label)", ns)
	}
	return nil
}

// MintOptions parameterizes Mint. Only KubeconfigPath and Namespaces are
// operator-facing; the rest are test seams and tuning.
type MintOptions struct {
	// KubeconfigPath is the HOSTED plane's admin kubeconfig
	// (cluster.control_plane_kubeconfig). Read locally, never embedded in the
	// output — the bundle carries only the least-privilege token derived
	// through it.
	KubeconfigPath string
	// Namespaces is the allow-list to provision and embed. Each namespace is
	// created on the plane when missing, so the policy the worker enforces
	// names namespaces that actually exist. Must be non-empty (fail-closed:
	// there is no "all namespaces" spelling on purpose).
	Namespaces []string

	// Client overrides the kubeconfig-built clientset — the deterministic-test
	// seam. When set, KubeconfigPath is not read.
	Client kubernetes.Interface
	// PollInterval / Timeout bound the wait for the token controller to
	// populate the token secret. Defaults: 250ms / 30s.
	PollInterval time.Duration
	Timeout      time.Duration
}

// vkClusterRoleRules is the FULL verb set vknode's runner uses, and nothing
// else — mirrored from internal/agent/vknode/runner.go:
//
//   - nodes / nodes/status: the node controller registers the virtual Node and
//     patches its status;
//   - pods (get/list/watch cluster-wide): the pod informer field-selects
//     spec.nodeName, which RBAC cannot express, so list/watch is cluster-wide
//     by necessity; delete completes graceful pod termination;
//   - pods/status: the pod controller reports phase transitions;
//   - configmaps/secrets/services (read-only): PodController's aux informers +
//     the native-artifact profile resolver;
//   - events: the event recorder.
//
// Deliberately absent: any write to secrets/configmaps, any RBAC or namespace
// verb, exec/attach/portforward subresources — a leaked bundle cannot mint
// further credentials or touch cluster policy.
var vkClusterRoleRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"nodes"},
		Verbs: []string{"create", "get", "list", "watch", "update", "patch"}},
	{APIGroups: []string{""}, Resources: []string{"nodes/status"},
		Verbs: []string{"update", "patch"}},
	{APIGroups: []string{""}, Resources: []string{"pods"},
		Verbs: []string{"get", "list", "watch", "delete"}},
	{APIGroups: []string{""}, Resources: []string{"pods/status"},
		Verbs: []string{"update", "patch"}},
	{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets", "services"},
		Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{""}, Resources: []string{"events"},
		Verbs: []string{"create", "update", "patch"}},
}

// managedLabels mark the minted objects as outpost-owned, for `kubectl get
// -l` legibility and so a future uninstall knows what it may delete.
var managedLabels = map[string]string{"outpost.io/managed": "true"}

// Mint provisions the vk identity on the hosted plane and returns the bundle.
//
// Idempotent: every ensure step tolerates the object already existing, and the
// ClusterRole is UPDATED to the canonical rule set on every call — so re-minting
// after an outpost upgrade converges drifted permissions instead of preserving
// stale ones. Re-minting returns the SAME token (the secret is reused), so
// handing the bundle to a second worker does not invalidate the first.
func Mint(ctx context.Context, opts MintOptions) (Bundle, error) {
	namespaces, err := normalizeNamespaces(opts.Namespaces)
	if err != nil {
		return Bundle{}, err
	}

	client := opts.Client
	if client == nil {
		if strings.TrimSpace(opts.KubeconfigPath) == "" {
			return Bundle{}, errors.New("vkcred: no kubeconfig path")
		}
		cfg, err := clientcmd.BuildConfigFromFlags("", opts.KubeconfigPath)
		if err != nil {
			return Bundle{}, fmt.Errorf("vkcred: load control-plane kubeconfig: %w", err)
		}
		client, err = kubernetes.NewForConfig(cfg)
		if err != nil {
			return Bundle{}, fmt.Errorf("vkcred: kube client: %w", err)
		}
	}

	// Provision the workload namespaces FIRST: the policy the bundle carries
	// must name namespaces that exist, or the worker's fail-closed gate admits
	// pods into namespaces the scheduler can never bind.
	for _, ns := range append([]string{SystemNamespace}, namespaces...) {
		if err := ensureNamespace(ctx, client, ns); err != nil {
			return Bundle{}, err
		}
	}
	if err := ensureServiceAccount(ctx, client); err != nil {
		return Bundle{}, err
	}
	if err := ensureClusterRole(ctx, client); err != nil {
		return Bundle{}, err
	}
	if err := ensureClusterRoleBinding(ctx, client); err != nil {
		return Bundle{}, err
	}
	token, ca, err := ensureTokenSecret(ctx, client, opts.PollInterval, opts.Timeout)
	if err != nil {
		return Bundle{}, err
	}

	b := Bundle{CA: ca, Token: token, Namespaces: namespaces}
	if err := b.Validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func normalizeNamespaces(in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, ns := range in {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if err := validateNamespaceName(ns); err != nil {
			return nil, err
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	if len(out) == 0 {
		return nil, errors.New("vkcred: at least one namespace required — the worker-side policy is fail-closed, so an empty list would deny every pod")
	}
	return out, nil
}

func ensureNamespace(ctx context.Context, client kubernetes.Interface, name string) error {
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: managedLabels},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("vkcred: ensure namespace %s: %w", name, err)
	}
	return nil
}

func ensureServiceAccount(ctx context.Context, client kubernetes.Interface) error {
	_, err := client.CoreV1().ServiceAccounts(SystemNamespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: ServiceAccountName, Namespace: SystemNamespace, Labels: managedLabels},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("vkcred: ensure service account: %w", err)
	}
	return nil
}

func ensureClusterRole(ctx context.Context, client kubernetes.Interface) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleName, Labels: managedLabels},
		Rules:      vkClusterRoleRules,
	}
	_, err := client.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Converge, don't preserve: the rule set is code, the object is cache.
		// Kubernetes requires resourceVersion on Update; fake clients often do
		// not, so constructing a fresh object here would pass unit tests and fail
		// against the real peer apiserver on the first drift repair.
		live, getErr := client.RbacV1().ClusterRoles().Get(ctx, ClusterRoleName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("vkcred: read existing cluster role: %w", getErr)
		}
		live.Labels = managedLabels
		live.Rules = vkClusterRoleRules
		_, err = client.RbacV1().ClusterRoles().Update(ctx, live, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("vkcred: ensure cluster role: %w", err)
	}
	return nil
}

func ensureClusterRoleBinding(ctx context.Context, client kubernetes.Interface) error {
	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleName, Labels: managedLabels},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ClusterRoleName},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: ServiceAccountName, Namespace: SystemNamespace,
		}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("vkcred: ensure cluster role binding: %w", err)
	}
	return nil
}

// ensureTokenSecret creates (or reuses) the service-account-token secret and
// waits for the plane's token controller to populate token + ca.crt.
//
// A LEGACY token secret rather than the TokenRequest API, deliberately: the
// worker persists the token in agent.json and re-materializes it across
// restarts, so a request-scoped token with a server-capped expiry would turn
// every apiserver restart gap into a re-mint. Rotation is explicit instead —
// delete the secret on the host and re-mint — matching how the other three
// join credentials rotate.
func ensureTokenSecret(ctx context.Context, client kubernetes.Interface, interval, timeout time.Duration) (token string, ca []byte, err error) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	_, err = client.CoreV1().Secrets(SystemNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      TokenSecretName,
			Namespace: SystemNamespace,
			Labels:    managedLabels,
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey: ServiceAccountName,
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return "", nil, fmt.Errorf("vkcred: create token secret: %w", err)
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		sec, err := client.CoreV1().Secrets(SystemNamespace).Get(ctx, TokenSecretName, metav1.GetOptions{})
		if err != nil {
			return "", nil, fmt.Errorf("vkcred: read token secret: %w", err)
		}
		token := string(sec.Data[corev1.ServiceAccountTokenKey])
		ca := sec.Data[corev1.ServiceAccountRootCAKey]
		if token != "" && len(ca) > 0 {
			return token, append([]byte(nil), ca...), nil
		}
		select {
		case <-ctx.Done():
			return "", nil, fmt.Errorf("vkcred: waiting for token secret: %w", ctx.Err())
		case <-deadline.C:
			return "", nil, errors.New("vkcred: token secret was not populated in time — is the control plane's token controller running?")
		case <-time.After(interval):
		}
	}
}
