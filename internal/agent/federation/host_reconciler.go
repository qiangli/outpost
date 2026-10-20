package federation

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	KarmadaGVR  = schema.GroupVersionResource{Group: "operator.karmada.io", Version: "v1alpha1", Resource: "karmadas"}
	ClustersGVR = schema.GroupVersionResource{Group: "cluster.karmada.io", Version: "v1alpha1", Resource: "clusters"}
)

// KarmadaCRName is the Karmada CR the builtin applies (design 9.3).
const KarmadaCRName = "karmada"

// KarmadaPhase is the observable state of the Karmada CR on the hosting plane.
type KarmadaPhase string

const (
	PhaseAbsent     KarmadaPhase = "absent"     // no CR (builtin not installed)
	PhaseInstalling KarmadaPhase = "installing" // CR present, not Ready
	PhaseReady      KarmadaPhase = "ready"
)

// Member is one Karmada Cluster object.
type Member struct {
	Name     string `json:"name"`
	SyncMode string `json:"sync_mode,omitempty"`
	Ready    bool   `json:"ready"`
}

// Plane reads the hosting DKS plane (the one running the Karmada operator).
type Plane interface {
	KarmadaPhase(ctx context.Context) (KarmadaPhase, error)
	AdminConfig(ctx context.Context) ([]byte, error)
}

// Karmada reads the Karmada control plane itself through the materialised
// kubeconfig.
type Karmada interface {
	Members(ctx context.Context) ([]Member, error)
}

// KubePlane is the client-go Plane.
type KubePlane struct {
	Client kubernetes.Interface
	Dyn    dynamic.Interface
}

func (p KubePlane) KarmadaPhase(ctx context.Context) (KarmadaPhase, error) {
	u, err := p.Dyn.Resource(KarmadaGVR).Namespace(KarmadaNamespace).Get(ctx, KarmadaCRName, metav1.GetOptions{})
	if err != nil {
		// A missing CRD surfaces as NotFound too: both mean "not installed".
		if apierrors.IsNotFound(err) {
			return PhaseAbsent, nil
		}
		return "", err
	}
	if conditionTrue(u, "Ready") {
		return PhaseReady, nil
	}
	return PhaseInstalling, nil
}

func (p KubePlane) AdminConfig(ctx context.Context) ([]byte, error) {
	return FetchAdminConfig(ctx, p.Client)
}

func conditionTrue(u *unstructured.Unstructured, typ string) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == typ {
			return m["status"] == "True"
		}
	}
	return false
}

// KubeKarmada lists Cluster objects through the federation kubeconfig.
type KubeKarmada struct{ Dyn dynamic.Interface }

// NewKubeKarmada builds a KubeKarmada from a kubeconfig path.
func NewKubeKarmada(kubeconfig string) (*KubeKarmada, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 5 * time.Second
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &KubeKarmada{Dyn: d}, nil
}

func (k *KubeKarmada) Members(ctx context.Context) ([]Member, error) {
	l, err := k.Dyn.Resource(ClustersGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(l.Items))
	for i := range l.Items {
		u := &l.Items[i]
		mode, _, _ := unstructured.NestedString(u.Object, "spec", "syncMode")
		out = append(out, Member{Name: u.GetName(), SyncMode: mode, Ready: conditionTrue(u, "Ready")})
	}
	return out, nil
}

// HostState is the reconciler's last observation, surfaced by `federation
// status` without re-probing.
type HostState struct {
	Phase          KarmadaPhase
	KubeconfigPath string
	CAHash         string
	SelfRegistered bool
	LastError      string
	LastRunAt      time.Time
}

// HostReconciler drives a federation host: install the builtin, materialise
// the kubeconfig, learn the CA hash, register the host's own plane. Every step
// is idempotent and a cold start only delays it (the retry-forever contract
// the control plane itself follows).
type HostReconciler struct {
	Plane           Plane
	Install         func(ctx context.Context) error // builtin install; idempotent
	KubeconfigPath  string
	Port            int
	ClusterName     string
	PlaneKubeconfig string // admin kubeconfig of the hosting plane (register --kubeconfig)
	Runner          Runner
	// Members lists registered clusters via the materialised kubeconfig.
	Members func(ctx context.Context, kubeconfig string) ([]Member, error)
	// RecordHash persists a newly learned CA hash.
	RecordHash func(hash string) error
	// PostRegister repoints the agent kubeconfig on the hosting plane.
	PostRegister func(ctx context.Context) error
	Interval     time.Duration
	Log          *slog.Logger

	mu    sync.Mutex
	state HostState
}

// State returns a copy of the last observation.
func (r *HostReconciler) State() HostState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *HostReconciler) set(f func(*HostState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.state)
}

// Reconcile runs one pass. It returns the first error but records it too.
func (r *HostReconciler) Reconcile(ctx context.Context) (err error) {
	defer func() {
		r.set(func(s *HostState) {
			s.LastRunAt = time.Now()
			s.LastError = ""
			if err != nil {
				s.LastError = err.Error()
			}
		})
	}()
	if err := CheckOS(); err != nil {
		return err
	}
	phase, err := r.Plane.KarmadaPhase(ctx)
	if err != nil {
		return fmt.Errorf("read Karmada CR: %w", err)
	}
	r.set(func(s *HostState) { s.Phase = phase })
	if phase == PhaseAbsent {
		if r.Install == nil {
			return fmt.Errorf("karmada builtin not installed and no installer wired")
		}
		if err := r.Install(ctx); err != nil {
			return fmt.Errorf("install karmada builtin: %w", err)
		}
		phase, err = r.Plane.KarmadaPhase(ctx)
		if err != nil {
			return err
		}
		r.set(func(s *HostState) { s.Phase = phase })
	}
	// The admin Secret exists as soon as the operator has stored status; do not
	// wait for full Ready to learn the identity.
	admin, err := r.Plane.AdminConfig(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // operator has not produced it yet — a delay, not a failure
		}
		return fmt.Errorf("read admin kubeconfig: %w", err)
	}
	rewritten, _, err := Materialize(r.KubeconfigPath, admin, r.Port)
	if err != nil {
		return fmt.Errorf("materialise kubeconfig: %w", err)
	}
	hash, err := CAHashFromKubeconfig(rewritten)
	if err != nil {
		return fmt.Errorf("compute CA hash: %w", err)
	}
	r.set(func(s *HostState) { s.KubeconfigPath = r.KubeconfigPath })
	if prev := r.State().CAHash; prev != hash {
		if r.RecordHash != nil {
			if err := r.RecordHash(hash); err != nil {
				return fmt.Errorf("record CA hash: %w", err)
			}
		}
		r.set(func(s *HostState) { s.CAHash = hash })
	}
	if phase != PhaseReady {
		return nil // register only against a Ready control plane
	}
	return r.ensureSelfRegistered(ctx)
}

func (r *HostReconciler) ensureSelfRegistered(ctx context.Context) error {
	if r.Members != nil {
		members, err := r.Members(ctx, r.KubeconfigPath)
		if err == nil {
			for _, m := range members {
				if m.Name == r.ClusterName {
					r.set(func(s *HostState) { s.SelfRegistered = true })
					return nil
				}
			}
			r.set(func(s *HostState) { s.SelfRegistered = false })
		}
		// A listing error usually means the loopback endpoint is not up yet;
		// try to register anyway only when we could list.
		if err != nil {
			return fmt.Errorf("list Karmada clusters: %w", err)
		}
	}
	if r.Runner == nil {
		return fmt.Errorf("no karmadactl runner wired")
	}
	if err := RegisterSelf(ctx, r.Runner, r.KubeconfigPath, r.PlaneKubeconfig, r.ClusterName, r.Port, r.PostRegister); err != nil {
		return fmt.Errorf("register host plane as %q: %w", r.ClusterName, err)
	}
	r.set(func(s *HostState) { s.SelfRegistered = true })
	return nil
}

// Run reconciles until ctx is cancelled, backing off 2s -> Interval on error.
func (r *HostReconciler) Run(ctx context.Context) {
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	wait := 2 * time.Second
	for {
		err := r.Reconcile(ctx)
		next := interval
		if err != nil {
			log.Info("federation host reconciler: not converged", "err", err, "retry_in", wait)
			next = wait
			if wait *= 2; wait > interval {
				wait = interval
			}
		} else {
			wait = 2 * time.Second
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
