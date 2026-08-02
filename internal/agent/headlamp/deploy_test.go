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

func TestDeployCreatesEverything(t *testing.T) {
	cs := fake.NewClientset()
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}

	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "headlamp", metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if _, err := cs.CoreV1().ServiceAccounts("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{}); err != nil {
		t.Fatalf("pod SA: %v", err)
	}
	if _, err := cs.CoreV1().ServiceAccounts("headlamp").Get(context.Background(), ViewerServiceAccount, metav1.GetOptions{}); err != nil {
		t.Fatalf("viewer SA: %v", err)
	}
	dep, err := cs.AppsV1().Deployments("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != "headlamp" {
		t.Fatalf("pod runs as %q, want the zero-grant SA", dep.Spec.Template.Spec.ServiceAccountName)
	}
	svc, err := cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("service type %s, want ClusterIP", svc.Spec.Type)
	}

	// The pod SA must have zero grants: the only bindings created are
	// the two viewer ones.
	crbs, _ := cs.RbacV1().ClusterRoleBindings().List(context.Background(), metav1.ListOptions{})
	if len(crbs.Items) != 2 {
		t.Fatalf("want exactly the 2 viewer bindings, got %d", len(crbs.Items))
	}
	for _, b := range crbs.Items {
		if subjectsInclude(b.Subjects, "headlamp", "headlamp") {
			t.Fatalf("binding %s references the pod SA — it must have zero grants", b.Name)
		}
	}
}

func TestDeployIsIdempotent(t *testing.T) {
	cs := fake.NewClientset()
	for i := range 2 {
		if err := Deploy(context.Background(), cs, Options{}); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
}

func TestDeployConvergesDriftedService(t *testing.T) {
	cs := fake.NewClientset()
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	// Drift: somebody flips the service to NodePort.
	svc, _ := cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	svc.Spec.Type = corev1.ServiceTypeNodePort
	svc.Spec.Ports[0].NodePort = 30080
	svc.Spec.ClusterIP = "10.43.0.99"
	if _, err := cs.CoreV1().Services("headlamp").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	svc, _ = cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("drifted service not converged: type %s", svc.Spec.Type)
	}
	if svc.Spec.Ports[0].NodePort != 0 {
		t.Fatalf("drifted nodePort survived: %d", svc.Spec.Ports[0].NodePort)
	}
	if svc.Spec.ClusterIP != "10.43.0.99" {
		t.Fatalf("allocated ClusterIP must be preserved, got %q", svc.Spec.ClusterIP)
	}
}

// TestDeployConvergesDriftedServicePreservesImmutableFields proves that
// a drifted-service convergence carries every immutable / server-defaulted
// field over from the existing object. The fake clientset does not reject
// an update that strips them (a real apiserver does), but this test
// asserts the code shape: the preserved fields are set before the Update
// call, which is the best deterministic gate short of envtest.
func TestDeployConvergesDriftedServicePreservesImmutableFields(t *testing.T) {
	cs := fake.NewClientset()
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	svc, _ := cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	svc.Spec.Type = corev1.ServiceTypeNodePort
	svc.Spec.Ports[0].NodePort = 30080
	svc.Spec.ClusterIP = "10.43.0.99"
	svc.Spec.ClusterIPs = []string{"10.43.0.99", "fd00::99"}
	svc.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}
	dualStack := corev1.IPFamilyPolicyRequireDualStack
	svc.Spec.IPFamilyPolicy = &dualStack
	if _, err := cs.CoreV1().Services("headlamp").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	svc, _ = cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{})
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("drifted type not converged: %s", svc.Spec.Type)
	}
	if len(svc.Spec.ClusterIPs) != 2 || svc.Spec.ClusterIPs[0] != "10.43.0.99" || svc.Spec.ClusterIPs[1] != "fd00::99" {
		t.Fatalf("ClusterIPs must be preserved, got %v", svc.Spec.ClusterIPs)
	}
	if len(svc.Spec.IPFamilies) != 2 || svc.Spec.IPFamilies[0] != corev1.IPv4Protocol || svc.Spec.IPFamilies[1] != corev1.IPv6Protocol {
		t.Fatalf("IPFamilies must be preserved, got %v", svc.Spec.IPFamilies)
	}
	if svc.Spec.IPFamilyPolicy == nil || *svc.Spec.IPFamilyPolicy != corev1.IPFamilyPolicyRequireDualStack {
		t.Fatalf("IPFamilyPolicy must be preserved, got %v", svc.Spec.IPFamilyPolicy)
	}
}

func TestDeployRefusesForeignNamespace(t *testing.T) {
	cs := fake.NewClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "headlamp"}, // no managed-by label
	})
	err := Deploy(context.Background(), cs, Options{})
	if err == nil || !strings.Contains(err.Error(), "not managed by outpost") {
		t.Fatalf("must refuse to adopt a foreign namespace, got %v", err)
	}
}

func TestDeployDisableViewerRBACRemovesViewerArtifacts(t *testing.T) {
	cs := fake.NewClientset()
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Deploy(context.Background(), cs, Options{DisableViewerRBAC: true}); err != nil {
		t.Fatal(err)
	}
	crbs, _ := cs.RbacV1().ClusterRoleBindings().List(context.Background(), metav1.ListOptions{})
	if len(crbs.Items) != 0 {
		t.Fatalf("viewer bindings must be removed, %d remain", len(crbs.Items))
	}
	if _, err := cs.RbacV1().ClusterRoles().Get(context.Background(), NodeViewClusterRole, metav1.GetOptions{}); err == nil {
		t.Fatal("nodeview clusterrole must be removed")
	}
	if _, err := cs.CoreV1().ServiceAccounts("headlamp").Get(context.Background(), ViewerServiceAccount, metav1.GetOptions{}); err == nil {
		t.Fatal("viewer SA must be removed")
	}
}

func TestDeployReplacesDriftedBindingRoleRef(t *testing.T) {
	cs := fake.NewClientset()
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	// Drift: the view binding is re-pointed at cluster-admin. roleRef
	// is immutable server-side, so convergence must delete + recreate.
	crb, _ := cs.RbacV1().ClusterRoleBindings().Get(context.Background(), "outpost-headlamp-viewer-view", metav1.GetOptions{})
	crb.RoleRef.Name = "cluster-admin"
	if _, err := cs.RbacV1().ClusterRoleBindings().Update(context.Background(), crb, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	crb, _ = cs.RbacV1().ClusterRoleBindings().Get(context.Background(), "outpost-headlamp-viewer-view", metav1.GetOptions{})
	if crb.RoleRef.Name != "view" {
		t.Fatalf("drifted roleRef survived: %s", crb.RoleRef.Name)
	}
}

func TestRemoveIsIdempotentAndScoped(t *testing.T) {
	cs := fake.NewClientset()
	if err := Deploy(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "headlamp", metav1.GetOptions{}); err == nil {
		t.Fatal("managed namespace must be deleted")
	}
	// Second Remove on empty state: fine.
	if err := Remove(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveLeavesForeignNamespace(t *testing.T) {
	cs := fake.NewClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-ns", Labels: map[string]string{"team": "x"}},
	})
	if err := Remove(context.Background(), cs, Options{Namespace: "shared-ns"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "shared-ns", metav1.GetOptions{}); err != nil {
		t.Fatal("a namespace outpost does not manage must never be deleted")
	}
}

func TestDeployFailsClosedOnAPIError(t *testing.T) {
	cs := fake.NewClientset()
	boom := errors.New("apiserver flake")
	cs.PrependReactor("create", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	err := Deploy(context.Background(), cs, Options{})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("API error must surface, got %v", err)
	}
}

// Guard: a binding that exists but references neither SA is left alone
// by the RBAC inspection helpers used across deploy/posture.
func TestSubjectsIncludeExactMatchOnly(t *testing.T) {
	subs := []rbacv1.Subject{
		{Kind: rbacv1.ServiceAccountKind, Namespace: "other", Name: "headlamp"},
		{Kind: "User", Namespace: "headlamp", Name: "headlamp"},
	}
	if subjectsInclude(subs, "headlamp", "headlamp") {
		t.Fatal("wrong-namespace or wrong-kind subjects must not match")
	}
}
