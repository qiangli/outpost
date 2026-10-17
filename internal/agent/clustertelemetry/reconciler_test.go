package clustertelemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type usageClient struct {
	kubernetes.Interface
	live kubernetes.Interface
}

// Use a real REST client only for the optional API; node/pod lists remain fake.
// CoreV1 has both list and REST operations, so wrap its typed interface.

type usageCore struct {
	typed.CoreV1Interface
	rest rest.Interface
}

func (c usageCore) RESTClient() rest.Interface { return c.rest }
func (c usageClient) CoreV1() typed.CoreV1Interface {
	return usageCore{c.Interface.CoreV1(), c.live.CoreV1().RESTClient()}
}

func TestExportSnapshot(t *testing.T) {
	for _, usage := range []bool{false, true} {
		t.Run(map[bool]string{false: "metrics-unavailable", true: "metrics-available"}[usage], func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !usage {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/apis/metrics.k8s.io/v1beta1/nodes":
					io.WriteString(w, `{"items":[{"metadata":{"name":"node-a"},"usage":{"cpu":"250m","memory":"2Mi"}}]}`)
				case "/apis/metrics.k8s.io/v1beta1/pods":
					io.WriteString(w, `{"items":[{"metadata":{"name":"pod-a","namespace":"default"},"containers":[{"usage":{"cpu":"100m","memory":"1Mi"}},{"usage":{"cpu":"150m","memory":"1Mi"}}]}]}`)
				default:
					t.Errorf("unexpected API path %s", r.URL.Path)
				}
			}))
			defer api.Close()
			live, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
			if err != nil {
				t.Fatal(err)
			}
			cs := fake.NewSimpleClientset(
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{corev1.LabelOSStable: "linux", "outpost.dhnt.io/backend": "k3s"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			)
			var got collector.ExportMetricsServiceRequest
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/metrics" {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing bearer")
				}
				b, _ := io.ReadAll(r.Body)
				if err := proto.Unmarshal(b, &got); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer receiver.Close()
			r := Reconciler{Client: usageClient{cs, live}}
			if err := r.Export(context.Background(), receiver.URL, "test-token"); err != nil {
				t.Fatal(err)
			}
			values := map[string]float64{}
			for _, rm := range got.ResourceMetrics {
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						for _, p := range m.GetGauge().DataPoints {
							attrs := map[string]string{}
							for _, a := range p.Attributes {
								attrs[a.Key] = a.Value.GetStringValue()
							}
							key := m.Name
							if key == "k8s.node.ready" {
								key += "/" + attrs["k8s.node.name"]
							}
							values[key] = p.GetAsDouble()
							if m.Name == "k8s.pod.phase" && (attrs["k8s.pod.name"] != "pod-a" || attrs["k8s.node.name"] != "node-a" || attrs["k8s.namespace.name"] != "default" || attrs["phase"] != "Running") {
								t.Errorf("pod attributes: %v", attrs)
							}
							if key == "k8s.node.ready/node-a" && (attrs["os"] != "linux" || attrs["backend"] != "k3s") {
								t.Errorf("node attributes: %v", attrs)
							}
						}
					}
				}
			}
			want := map[string]float64{"k8s.node.ready/node-a": 1, "k8s.node.ready/node-b": 0, "k8s.pod.phase": 1}
			if usage {
				want["k8s.node.cpu.usage"] = 0.25
				want["k8s.pod.cpu.usage"] = 0.25
				want["k8s.node.memory.usage"] = 2097152
				want["k8s.pod.memory.usage"] = 2097152
			}
			if len(values) != len(want) {
				t.Fatalf("metrics=%v want %v", values, want)
			}
			for k, v := range want {
				if actual, ok := values[k]; !ok || actual != v {
					t.Errorf("%s=%v present=%v want %v", k, actual, ok, v)
				}
			}
			// A second export must not retain deleted objects or the previous phase.
			if err := cs.CoreV1().Pods("default").Delete(context.Background(), "pod-a", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := r.Export(context.Background(), receiver.URL, "test-token"); err != nil {
				t.Fatal(err)
			}
			for _, rm := range got.ResourceMetrics {
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						if m.Name == "k8s.pod.phase" {
							t.Error("deleted pod retained")
						}
					}
				}
			}
		})
	}
}

func TestInactiveUnitDoesNotReadPlane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cs := fake.NewSimpleClientset()
	r := Reconciler{Client: cs, Target: func() (string, string, bool) { cancel(); return "", "", false }}
	r.Run(ctx)
	if len(cs.Actions()) != 0 {
		t.Fatalf("inactive unit accessed Kubernetes: %v", cs.Actions())
	}
}
