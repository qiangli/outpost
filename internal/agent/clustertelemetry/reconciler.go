// Package clustertelemetry exports snapshots of a hosted Kubernetes plane.
package clustertelemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/qiangli/outpost/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	corev1 "k8s.io/api/core/v1"
	kresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Reconciler checks the unit on every tick so activation and deactivation take
// effect without restarting the daemon. Target returns false for an inactive unit.
type Reconciler struct {
	Client kubernetes.Interface
	Target func() (endpoint, token string, active bool)
	Log    *slog.Logger
}

func (r *Reconciler) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		endpoint, token, active := r.Target()
		if active {
			err := r.Export(ctx, endpoint, token)
			if err != nil && ctx.Err() == nil && r.Log != nil {
				r.Log.Warn("cluster telemetry export failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Export emits only current samples: deleted pods and old phases are not retained.
func (r *Reconciler) Export(ctx context.Context, endpoint, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	nodes, err := r.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	pods, err := r.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	now := time.Now()
	metrics := []metricdata.Metrics{}
	indices := map[string]int{}
	add := func(name, unit string, value float64, attrs ...attribute.KeyValue) {
		point := metricdata.DataPoint[float64]{Attributes: attribute.NewSet(attrs...), Time: now, Value: value}
		if i, ok := indices[name]; ok {
			gauge := metrics[i].Data.(metricdata.Gauge[float64])
			gauge.DataPoints = append(gauge.DataPoints, point)
			metrics[i].Data = gauge
			return
		}
		indices[name] = len(metrics)
		metrics = append(metrics, metricdata.Metrics{Name: name, Unit: unit, Data: metricdata.Gauge[float64]{DataPoints: []metricdata.DataPoint[float64]{point}}})
	}

	for _, n := range nodes.Items {
		ready := 0.0
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = 1
			}
		}
		add("k8s.node.ready", "1", ready, attribute.String("k8s.node.name", n.Name), attribute.String("os", n.Labels[corev1.LabelOSStable]), attribute.String("backend", n.Labels["outpost.dhnt.io/backend"]))
	}
	for _, p := range pods.Items {
		add("k8s.pod.phase", "1", 1, attribute.String("k8s.namespace.name", p.Namespace), attribute.String("k8s.pod.name", p.Name), attribute.String("k8s.node.name", p.Spec.NodeName), attribute.String("phase", string(p.Status.Phase)))
	}
	// Use the existing REST client for the optional aggregated metrics API;
	// this avoids adding another Kubernetes client module.
	type usage map[corev1.ResourceName]kresource.Quantity
	type item struct {
		Metadata   metav1.ObjectMeta `json:"metadata"`
		Usage      usage             `json:"usage"`
		Containers []struct {
			Usage usage `json:"usage"`
		} `json:"containers"`
	}
	rest := r.Client.CoreV1().RESTClient()
	if rest != nil {
		for _, kind := range []string{"nodes", "pods"} {
			var list struct {
				Items []item `json:"items"`
			}
			// Bound optional API delays so readiness/phase export can still complete.
			usageCtx, usageCancel := context.WithTimeout(ctx, 2*time.Second)
			raw, e := rest.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/"+kind).SetHeader("Accept", "application/json").DoRaw(usageCtx)
			usageCancel()
			if e != nil || json.Unmarshal(raw, &list) != nil {
				continue
			}
			for _, m := range list.Items {
				prefix := "k8s.node"
				attrs := []attribute.KeyValue{attribute.String("k8s.node.name", m.Metadata.Name)}
				usages := []usage{m.Usage}
				if kind == "pods" {
					prefix = "k8s.pod"
					node := ""
					for _, p := range pods.Items {
						if p.Name == m.Metadata.Name && p.Namespace == m.Metadata.Namespace {
							node = p.Spec.NodeName
							break
						}
					}
					attrs = []attribute.KeyValue{attribute.String("k8s.namespace.name", m.Metadata.Namespace), attribute.String("k8s.pod.name", m.Metadata.Name), attribute.String("k8s.node.name", node)}
					usages = nil
					for _, c := range m.Containers {
						usages = append(usages, c.Usage)
					}
				}
				for _, res := range []struct {
					key          corev1.ResourceName
					suffix, unit string
				}{{corev1.ResourceCPU, "cpu.usage", "{cpu}"}, {corev1.ResourceMemory, "memory.usage", "By"}} {
					total, present := 0.0, false
					for _, u := range usages {
						if q, ok := u[res.key]; ok {
							total += q.AsApproximateFloat64()
							present = true
						}
					}
					if present {
						add(prefix+"."+res.suffix, res.unit, total, attrs...)
					}
				}
			}
		}
	}
	return telemetry.ExportGauges(ctx, endpoint, token, &metricdata.ResourceMetrics{Resource: resource.NewSchemaless(attribute.String("service.name", "outpost")), ScopeMetrics: []metricdata.ScopeMetrics{{Scope: instrumentation.Scope{Name: "outpost.cluster"}, Metrics: metrics}}})
}
