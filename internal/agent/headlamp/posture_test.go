package headlamp

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// deployed returns a clientset holding a freshly Deployed headlamp with
// one ready replica reported.
func deployed(t *testing.T) *fake.Clientset {
	t.Helper()
	cs := fake.NewClientset()
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	dep, _ := cs.AppsV1().Deployments("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	dep.Status.ReadyReplicas = 1
	if _, err := cs.AppsV1().Deployments("headlamp").UpdateStatus(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	return cs
}

func mustInspect(t *testing.T, cs *fake.Clientset) *Posture {
	t.Helper()
	p, err := Inspect(context.Background(), cs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInspectCleanDeploymentIsSecure(t *testing.T) {
	p := mustInspect(t, deployed(t))
	if !p.Running() {
		t.Fatalf("want Running, got %+v", p)
	}
	if !p.Secure() {
		t.Fatalf("want Secure, violations: %v", p.Violations)
	}
	if p.ServiceType != corev1.ServiceTypeClusterIP {
		t.Fatalf("service type %s", p.ServiceType)
	}
}

func TestInspectAbsentIsNotRunningNotSecure(t *testing.T) {
	p := mustInspect(t, fake.NewClientset())
	if p.Running() || p.Secure() {
		t.Fatalf("empty cluster must be neither Running nor Secure: %+v", p)
	}
}

func TestInspectFlagsNodePortService(t *testing.T) {
	cs := deployed(t)
	svc, _ := cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	svc.Spec.Type = corev1.ServiceTypeNodePort
	svc.Spec.Ports[0].NodePort = 30080
	if _, err := cs.CoreV1().Services("headlamp").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := mustInspect(t, cs)
	if p.Secure() {
		t.Fatal("NodePort service must not be Secure")
	}
	joined := strings.Join(p.Violations, "; ")
	if !strings.Contains(joined, "NodePort") || !strings.Contains(joined, "30080") {
		t.Fatalf("violations must name the exposure: %v", p.Violations)
	}
}

func TestInspectFlagsExternalIPs(t *testing.T) {
	cs := deployed(t)
	svc, _ := cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	svc.Spec.ExternalIPs = []string{"192.168.1.50"}
	if _, err := cs.CoreV1().Services("headlamp").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := mustInspect(t, cs)
	if p.Secure() {
		t.Fatal("externalIPs must not be Secure")
	}
}

func TestInspectFlagsHostNetworkPod(t *testing.T) {
	cs := deployed(t)
	priv := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "headlamp-abc",
			Namespace: "headlamp",
			Labels:    map[string]string{nameLabel: "headlamp"},
		},
		Spec: corev1.PodSpec{
			HostNetwork:        true,
			ServiceAccountName: "headlamp",
			Containers: []corev1.Container{{
				Name:            "headlamp",
				Ports:           []corev1.ContainerPort{{ContainerPort: 4466, HostPort: 4466}},
				SecurityContext: &corev1.SecurityContext{Privileged: &priv},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods("headlamp").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := mustInspect(t, cs)
	joined := strings.Join(p.Violations, "; ")
	for _, want := range []string{"hostNetwork", "hostPort 4466", "privileged"} {
		if !strings.Contains(joined, want) {
			t.Errorf("violations must mention %q: %v", want, p.Violations)
		}
	}
}

func TestInspectFlagsPodSAWithAnyGrant(t *testing.T) {
	cs := deployed(t)
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "sneaky-admin"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Namespace: "headlamp", Name: "headlamp",
		}},
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(context.Background(), crb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := mustInspect(t, cs)
	if p.Secure() {
		t.Fatal("a grant on the pod SA must never be Secure")
	}
	if !strings.Contains(strings.Join(p.Violations, "; "), "zero grants") {
		t.Fatalf("violation must state the zero-grant rule: %v", p.Violations)
	}
}

func TestInspectFlagsPodSANamespacedGrant(t *testing.T) {
	cs := deployed(t)
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "sneaky-ns", Namespace: "kube-system"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "secrets-reader"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Namespace: "headlamp", Name: "headlamp",
		}},
	}
	if _, err := cs.RbacV1().RoleBindings("kube-system").Create(context.Background(), rb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := mustInspect(t, cs)
	if p.Secure() {
		t.Fatal("a namespaced grant on the pod SA must never be Secure")
	}
}

func TestInspectFlagsWidenedViewerBinding(t *testing.T) {
	cs := deployed(t)
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer-becomes-admin"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Namespace: "headlamp", Name: ViewerServiceAccount,
		}},
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(context.Background(), crb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := mustInspect(t, cs)
	if p.Secure() {
		t.Fatal("an extra viewer grant must never be Secure")
	}
}

func TestInspectIgnoresUnrelatedBindings(t *testing.T) {
	cs := deployed(t)
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Namespace: "kube-system", Name: "somebody-else",
		}},
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(context.Background(), crb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if p := mustInspect(t, cs); !p.Secure() {
		t.Fatalf("bindings not referencing our SAs must be ignored: %v", p.Violations)
	}
}

func TestInspectFlagsForeignServiceAccountPod(t *testing.T) {
	cs := deployed(t)
	dep, _ := cs.AppsV1().Deployments("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	dep.Spec.Template.Spec.ServiceAccountName = "default"
	if _, err := cs.AppsV1().Deployments("headlamp").Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := mustInspect(t, cs)
	if p.Secure() {
		t.Fatal("a template running as another SA must not be Secure")
	}
}

// Fail-closed: an apiserver error while auditing must surface as an
// error, never as a (possibly "secure") Posture.
func TestInspectFailsClosedOnAPIError(t *testing.T) {
	cs := deployed(t)
	boom := errors.New("apiserver flake")
	cs.PrependReactor("list", "clusterrolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	p, err := Inspect(context.Background(), cs, Options{})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want the API error surfaced, got p=%v err=%v", p, err)
	}
	if p != nil {
		t.Fatal("no Posture may be returned alongside an audit error")
	}
}
