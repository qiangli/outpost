package federation

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// Member-plane mirror of the Karmada apiserver (design §5.3). On a member,
// karmada-agent dials https://karmada-apiserver.karmada-system.svc:5443 — the
// name the Karmada cert carries as a SAN — so the member plane gets a
// selectorless Service of that name whose Endpoints point at the federation
// visitor inside this host's runtime container. Kubernetes rejects loopback
// Endpoints, so the address is the node's InternalIP (the container's eth0),
// re-read on every pass because a recreated container can come back on a new
// address.

const (
	mirrorPortName   = "https"
	mirrorManagedBy  = "app.kubernetes.io/managed-by"
	mirrorManagerVal = "outpost-federation"
)

// NodeInternalIP returns the node's first usable InternalIP: IPv4 preferred,
// never loopback, link-local or unspecified (all of which Endpoints reject).
func NodeInternalIP(node *corev1.Node) (string, error) {
	var fallback string
	for _, a := range node.Status.Addresses {
		if a.Type != corev1.NodeInternalIP {
			continue
		}
		ip := net.ParseIP(a.Address)
		if ip == nil || !usableEndpointIP(ip) {
			continue
		}
		if ip.To4() != nil {
			return ip.String(), nil
		}
		if fallback == "" {
			fallback = ip.String()
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("node %q has no usable (non-loopback) InternalIP", node.Name)
}

func usableEndpointIP(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

// EnsureMirror converges the mirror Service + Endpoints on the member plane and
// returns the endpoint address it points at. An Endpoints object carrying any
// other address — a loopback one in particular — is replaced. A same-named
// Service WITH a selector is the real operator Service (this plane hosts
// Karmada) and is never touched.
func EnsureMirror(ctx context.Context, cs kubernetes.Interface, nodeName string, port int) (string, error) {
	node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read node %q: %w", nodeName, err)
	}
	ip, err := NodeInternalIP(node)
	if err != nil {
		return "", err
	}
	if err := ensureNamespace(ctx, cs); err != nil {
		return "", err
	}
	if err := ensureMirrorService(ctx, cs, port); err != nil {
		return "", err
	}
	if err := ensureMirrorEndpoints(ctx, cs, ip, port); err != nil {
		return "", err
	}
	return ip, nil
}

func ensureNamespace(ctx context.Context, cs kubernetes.Interface) error {
	_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: KarmadaNamespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", KarmadaNamespace, err)
	}
	return nil
}

func mirrorService(port int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: APIServerServiceName, Namespace: KarmadaNamespace,
			Labels: map[string]string{mirrorManagedBy: mirrorManagerVal},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{
				Name: mirrorPortName, Protocol: corev1.ProtocolTCP,
				Port: APIServerServicePort, TargetPort: intstr.FromInt32(int32(port)),
			}},
		},
	}
}

func ensureMirrorService(ctx context.Context, cs kubernetes.Interface, port int) error {
	svcs := cs.CoreV1().Services(KarmadaNamespace)
	want := mirrorService(port)
	cur, err := svcs.Get(ctx, APIServerServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = svcs.Create(ctx, want, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if len(cur.Spec.Selector) > 0 {
		return fmt.Errorf("service %s/%s has a selector: this plane hosts the Karmada apiserver itself and needs no mirror", KarmadaNamespace, APIServerServiceName)
	}
	if len(cur.Spec.Ports) == 1 && cur.Spec.Ports[0].Name == mirrorPortName &&
		cur.Spec.Ports[0].Port == APIServerServicePort && cur.Spec.Ports[0].TargetPort.IntValue() == port {
		return nil
	}
	cur.Spec.Ports = want.Spec.Ports
	_, err = svcs.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

func mirrorSubsets(ip string, port int) []corev1.EndpointSubset {
	return []corev1.EndpointSubset{{
		Addresses: []corev1.EndpointAddress{{IP: ip}},
		Ports:     []corev1.EndpointPort{{Name: mirrorPortName, Port: int32(port), Protocol: corev1.ProtocolTCP}},
	}}
}

func subsetsMatch(got []corev1.EndpointSubset, ip string, port int) bool {
	if len(got) != 1 || len(got[0].Addresses) != 1 || len(got[0].NotReadyAddresses) != 0 || len(got[0].Ports) != 1 {
		return false
	}
	p := got[0].Ports[0]
	return got[0].Addresses[0].IP == ip && p.Name == mirrorPortName && p.Port == int32(port)
}

//nolint:staticcheck // Endpoints (not EndpointSlice) is the design's object: a selectorless Service's EndpointSlice is mirrored from it.
func ensureMirrorEndpoints(ctx context.Context, cs kubernetes.Interface, ip string, port int) error {
	eps := cs.CoreV1().Endpoints(KarmadaNamespace)
	cur, err := eps.Get(ctx, APIServerServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = eps.Create(ctx, &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name: APIServerServiceName, Namespace: KarmadaNamespace,
				Labels: map[string]string{mirrorManagedBy: mirrorManagerVal},
			},
			Subsets: mirrorSubsets(ip, port),
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if subsetsMatch(cur.Subsets, ip, port) {
		return nil
	}
	cur.Subsets = mirrorSubsets(ip, port)
	_, err = eps.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

// RemoveMirror deletes the mirror Service + Endpoints. Only objects this
// package created (managed-by label) are deleted; missing ones are fine.
//
//nolint:staticcheck // see ensureMirrorEndpoints
func RemoveMirror(ctx context.Context, cs kubernetes.Interface) error {
	svc, err := cs.CoreV1().Services(KarmadaNamespace).Get(ctx, APIServerServiceName, metav1.GetOptions{})
	if err == nil && svc.Labels[mirrorManagedBy] == mirrorManagerVal {
		if err := cs.CoreV1().Services(KarmadaNamespace).Delete(ctx, APIServerServiceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	ep, err := cs.CoreV1().Endpoints(KarmadaNamespace).Get(ctx, APIServerServiceName, metav1.GetOptions{})
	if err == nil && ep.Labels[mirrorManagedBy] == mirrorManagerVal {
		if err := cs.CoreV1().Endpoints(KarmadaNamespace).Delete(ctx, APIServerServiceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// MemberReconciler drives a federation member: keep the mirror pointed at the
// node's InternalIP, finish a pending join (wait for the visitor, register,
// fixups, delete the bootstrap token) and keep the fixups applied — karmadactl
// or an operator re-registering would otherwise re-bake 127.0.0.1 into the
// agent's kubeconfig.
type MemberReconciler struct {
	Client          kubernetes.Interface // member plane admin client
	NodeName        string               // the runtime container's node
	ClusterName     string
	PlaneKubeconfig string // member admin kubeconfig (karmadactl --kubeconfig)
	CAHash          string
	Port            int
	PendingPath     string
	Runner          Runner
	// Dial probes the loopback visitor; nil means DialVisitor.
	Dial     func(ctx context.Context, port int) error
	Interval time.Duration
	Log      *slog.Logger

	mu    sync.Mutex
	state HostState
}

// State returns a copy of the last observation.
func (r *MemberReconciler) State() HostState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *MemberReconciler) set(f func(*HostState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.state)
}

// Reconcile runs one pass and records its error.
func (r *MemberReconciler) Reconcile(ctx context.Context) (err error) {
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
	dial := r.Dial
	if dial == nil {
		dial = DialVisitor
	}
	bound := dial(ctx, r.Port) == nil
	r.set(func(s *HostState) { s.VisitorBound = bound })

	ip, err := EnsureMirror(ctx, r.Client, r.NodeName, r.Port)
	if err != nil {
		return fmt.Errorf("mirror service: %w", err)
	}
	r.set(func(s *HostState) { s.MirrorIP = ip })

	pending, ok, err := ReadPendingJoin(r.PendingPath)
	if err != nil {
		return err
	}
	r.set(func(s *HostState) { s.JoinPending = ok })
	if ok {
		if !bound {
			return fmt.Errorf("waiting for the federation visitor on %s", LoopbackEndpoint(r.Port))
		}
		if r.Runner == nil {
			return fmt.Errorf("no karmadactl runner wired")
		}
		if err := RegisterMember(ctx, r.Runner, LoopbackEndpoint(r.Port), pending.BootstrapToken, r.CAHash, r.ClusterName, r.PlaneKubeconfig); err != nil {
			return fmt.Errorf("register as %q: %w", r.ClusterName, err)
		}
		if err := RemovePendingJoin(r.PendingPath); err != nil {
			return fmt.Errorf("remove pending join: %w", err)
		}
		r.set(func(s *HostState) { s.JoinPending = false })
	}
	if _, err := applyFixups(ctx, r.Client, r.NodeName); err != nil {
		return err
	}
	exists, ready, err := AgentReady(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("read karmada-agent: %w", err)
	}
	r.set(func(s *HostState) { s.SelfRegistered, s.AgentReady = exists, ready })
	return nil
}

// Run reconciles every Interval (default 30 s) until ctx is cancelled,
// retrying sooner (2 s doubling) while not converged.
func (r *MemberReconciler) Run(ctx context.Context) {
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
		next := interval
		if err := r.Reconcile(ctx); err != nil {
			log.Info("federation member reconciler: not converged", "err", err, "retry_in", wait)
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
