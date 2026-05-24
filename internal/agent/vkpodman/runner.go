package vkpodman

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
)

// RunOptions configures one vkpodman cluster-join lifetime. Callers
// build it from either a kubeconfig file (the cmd/outpost-vk PoC) or
// from the persisted conf.ClusterConfig (the main outpost startCmd
// path); the runner doesn't care which.
type RunOptions struct {
	// NodeName is the identity to register with the apiserver — what
	// `kubectl get nodes` will show. Typically the outpost's AgentName.
	NodeName string

	// PodmanSocket is the unix socket path to the local libpod daemon.
	// Callers usually obtain it from agent.DetectPodman().
	PodmanSocket string

	// Kube is the already-built kube REST config. Caller is responsible
	// for plumbing the bearer token / CA — see ConfigFromCluster or
	// clientcmd.BuildConfigFromFlags.
	Kube *rest.Config

	// ExtraNodeLabels are merged into the registered Node's Labels map.
	// Useful for nodeSelector targeting (e.g. {"outpost.dhnt.io/gpu":"true"}).
	ExtraNodeLabels map[string]string
}

// Run blocks until ctx is canceled (or any sub-controller errors out),
// running:
//
//   - the libpod-backed Provider,
//   - the virtual-kubelet NodeController (drives the Node lease /
//     status updates),
//   - the virtual-kubelet PodController (watches Pods assigned to this
//     node and translates them to libpod calls),
//   - the SharedInformerFactories backing the controllers.
//
// Returns nil on a clean ctx-canceled shutdown; non-nil for any setup
// or runtime error the caller should surface.
func Run(ctx context.Context, opts RunOptions) error {
	if opts.NodeName == "" {
		return errors.New("vkpodman: NodeName required")
	}
	if opts.PodmanSocket == "" {
		return errors.New("vkpodman: PodmanSocket required")
	}
	if opts.Kube == nil {
		return errors.New("vkpodman: Kube REST config required")
	}

	prov, err := NewProvider(opts.PodmanSocket)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	if err := prov.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	client, err := kubernetes.NewForConfig(opts.Kube)
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}

	// Pod informer scoped to "pods assigned to this node" — we don't
	// care about pods scheduled elsewhere, and watching them would burn
	// memory on a busy cluster.
	nodeFilter := func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", opts.NodeName).String()
	}
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		client, 0,
		informers.WithNamespace(corev1.NamespaceAll),
		informers.WithTweakListOptions(nodeFilter),
	)
	// ConfigMap/Secret/Service informers are required by PodController
	// even when we don't resolve downward-API refs (translate.go rejects
	// EnvFrom in v1). Empty lister results are fine.
	auxFactory := informers.NewSharedInformerFactoryWithOptions(client, 0)

	rec := newEventRecorder(client.CoreV1())

	nodeObj := BuildNode(opts.NodeName, opts.ExtraNodeLabels)
	nc, err := node.NewNodeController(
		NewNodeProvider(prov.Client(), nodeObj),
		nodeObj,
		client.CoreV1().Nodes(),
	)
	if err != nil {
		return fmt.Errorf("node controller: %w", err)
	}

	pc, err := node.NewPodController(node.PodControllerConfig{
		PodClient:         client.CoreV1(),
		PodInformer:       podInformerFactory.Core().V1().Pods(),
		EventRecorder:     rec,
		Provider:          prov,
		ConfigMapInformer: auxFactory.Core().V1().ConfigMaps(),
		SecretInformer:    auxFactory.Core().V1().Secrets(),
		ServiceInformer:   auxFactory.Core().V1().Services(),
	})
	if err != nil {
		return fmt.Errorf("pod controller: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		podInformerFactory.Start(gctx.Done())
		auxFactory.Start(gctx.Done())
		<-gctx.Done()
		return nil
	})
	g.Go(func() error {
		slog.Info("vkpodman: starting node controller", "node", opts.NodeName)
		err := nc.Run(gctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("vkpodman: node controller exited", "err", err)
			return err
		}
		slog.Info("vkpodman: node controller exited")
		return nil
	})
	g.Go(func() error {
		slog.Info("vkpodman: starting pod controller", "node", opts.NodeName)
		// The pod controller can't safely act until the node has
		// registered with the apiserver — block on the node
		// controller's Ready signal before starting the workers.
		select {
		case <-nc.Ready():
		case <-gctx.Done():
			return nil
		}
		slog.Info("vkpodman: node ready, starting pod controller workers")
		err := pc.Run(gctx, 1)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("vkpodman: pod controller exited", "err", err)
			return err
		}
		slog.Info("vkpodman: pod controller exited")
		return nil
	})

	slog.Info("vkpodman: running",
		"node", opts.NodeName,
		"podman_socket", opts.PodmanSocket,
		"apiserver", opts.Kube.Host)
	err = g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ConfigFromCluster builds a kube REST config that reads the bearer
// token from tokenFile rather than baking it into the config. This is
// the load-bearing detail that makes token rotation work without
// rebuilding the entire client-go stack: the transport re-reads
// tokenFile on its own schedule, so a Refresher writing a fresh token
// to the same file picks up without the controllers noticing.
//
// The cmd/outpost-vk PoC uses clientcmd.BuildConfigFromFlags directly
// because its kubeconfig already inlines a static token (no
// rotation); only the main agent path goes through this builder.
func ConfigFromCluster(apiURL, tokenFile string, caPEM []byte) (*rest.Config, error) {
	if apiURL == "" {
		return nil, errors.New("vkpodman: empty cluster APIURL")
	}
	if tokenFile == "" {
		return nil, errors.New("vkpodman: empty cluster tokenFile path")
	}
	cfg := &rest.Config{
		Host:            apiURL,
		BearerTokenFile: tokenFile,
	}
	if len(caPEM) > 0 {
		cfg.TLSClientConfig.CAData = append([]byte(nil), caPEM...)
	}
	return cfg, nil
}

// newEventRecorder wires a broadcaster that prints events to slog (so
// they're visible in the agent's logs) and also surfaces them on the
// apiserver via the core/v1 Events API. The recorder is required by
// PodController.
func newEventRecorder(events typedcorev1.EventsGetter) record.EventRecorder {
	b := record.NewBroadcaster()
	b.StartStructuredLogging(0)
	b.StartRecordingToSink(&apiserverEventSink{events: events})
	return b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "outpost-vkpodman"})
}

// apiserverEventSink forwards recorded events to core/v1 Events. The
// record.EventSink interface is small (Create/Update/Patch); Patch is
// unused by PodController so we leave it unimplemented.
type apiserverEventSink struct {
	events typedcorev1.EventsGetter
}

func (s *apiserverEventSink) Create(e *corev1.Event) (*corev1.Event, error) {
	return s.events.Events(e.Namespace).Create(context.TODO(), e, metav1.CreateOptions{})
}
func (s *apiserverEventSink) Update(e *corev1.Event) (*corev1.Event, error) {
	return s.events.Events(e.Namespace).Update(context.TODO(), e, metav1.UpdateOptions{})
}
func (s *apiserverEventSink) Patch(_ *corev1.Event, _ []byte) (*corev1.Event, error) {
	return nil, errors.New("apiserverEventSink: patch not implemented")
}
