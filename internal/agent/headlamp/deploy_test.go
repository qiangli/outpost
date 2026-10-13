package headlamp

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func seedTestCluster(t *testing.T, cs *fake.Clientset, o Options) {
	t.Helper()
	o, err := o.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(), o.namespaceObject(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(o.Namespace).Create(context.Background(), o.podServiceAccount(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(o.Namespace).Create(context.Background(), o.viewerServiceAccount(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.RbacV1().ClusterRoles().Create(context.Background(), o.nodeViewClusterRole(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(context.Background(), o.viewerBinding(viewerBindingView, "view"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(context.Background(), o.viewerBinding(viewerBindingNodes, NodeViewClusterRole), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dep := o.deployment()
	dep.Status.ReadyReplicas = 1
	if _, err := cs.AppsV1().Deployments(o.Namespace).Create(context.Background(), dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Services(o.Namespace).Create(context.Background(), o.service(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestDeployRefusesDisabled_CreatesNoObjects(t *testing.T) {
	cs := fake.NewClientset()
	err := Deploy(context.Background(), cs, Options{})
	if err == nil {
		t.Fatal("Deploy must fail closed")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
	wantMsg := "headlamp is disabled by the service supply-chain policy (no bashy-signed build yet; blocked on upstream fixes for GO-2026-5064/5338/5622/5932)"
	if err.Error() != wantMsg {
		t.Fatalf("err.Error() = %q, want %q", err.Error(), wantMsg)
	}

	// Deploy must touch nothing on the cluster before refusing.
	nsList, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil || len(nsList.Items) != 0 {
		t.Fatalf("expected 0 namespaces, got %d", len(nsList.Items))
	}
	depList, err := cs.AppsV1().Deployments("").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(depList.Items) != 0 {
		t.Fatalf("expected 0 deployments, got %d", len(depList.Items))
	}
	svcList, err := cs.CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(svcList.Items) != 0 {
		t.Fatalf("expected 0 services, got %d", len(svcList.Items))
	}
	saList, err := cs.CoreV1().ServiceAccounts("").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(saList.Items) != 0 {
		t.Fatalf("expected 0 serviceaccounts, got %d", len(saList.Items))
	}
	crbList, err := cs.RbacV1().ClusterRoleBindings().List(context.Background(), metav1.ListOptions{})
	if err != nil || len(crbList.Items) != 0 {
		t.Fatalf("expected 0 clusterrolebindings, got %d", len(crbList.Items))
	}
	crList, err := cs.RbacV1().ClusterRoles().List(context.Background(), metav1.ListOptions{})
	if err != nil || len(crList.Items) != 0 {
		t.Fatalf("expected 0 clusterroles, got %d", len(crList.Items))
	}
}

func TestDeployRefusesDisabled_ImageOverrideDoesNotBypass(t *testing.T) {
	cs := fake.NewClientset()
	err := Deploy(context.Background(), cs, Options{Image: "example.org/custom/headlamp:latest"})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Options.Image override must NOT bypass refusal, got %v", err)
	}
	depList, err := cs.AppsV1().Deployments("").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(depList.Items) != 0 {
		t.Fatalf("expected 0 deployments, got %d", len(depList.Items))
	}
}

func TestDeployRefusesDisabled_NilClientset(t *testing.T) {
	err := Deploy(context.Background(), nil, Options{})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled even with nil clientset, got %v", err)
	}
}

func TestRemoveIsIdempotentAndScoped(t *testing.T) {
	cs := fake.NewClientset()
	seedTestCluster(t, cs, Options{})
	if err := Remove(context.Background(), cs, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "headlamp", metav1.GetOptions{}); err == nil {
		t.Fatal("managed namespace must be deleted")
	}
	if _, err := cs.AppsV1().Deployments("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{}); err == nil {
		t.Fatal("deployment must be deleted")
	}
	if _, err := cs.CoreV1().Services("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{}); err == nil {
		t.Fatal("service must be deleted")
	}
	if _, err := cs.CoreV1().ServiceAccounts("headlamp").Get(context.Background(), "headlamp", metav1.GetOptions{}); err == nil {
		t.Fatal("pod SA must be deleted")
	}
	if _, err := cs.CoreV1().ServiceAccounts("headlamp").Get(context.Background(), ViewerServiceAccount, metav1.GetOptions{}); err == nil {
		t.Fatal("viewer SA must be deleted")
	}
	crbs, _ := cs.RbacV1().ClusterRoleBindings().List(context.Background(), metav1.ListOptions{})
	if len(crbs.Items) != 0 {
		t.Fatalf("viewer bindings must be deleted, %d remain", len(crbs.Items))
	}
	if _, err := cs.RbacV1().ClusterRoles().Get(context.Background(), NodeViewClusterRole, metav1.GetOptions{}); err == nil {
		t.Fatal("nodeview clusterrole must be deleted")
	}
	// Second Remove on empty state: fine (idempotent).
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

func TestSubjectsIncludeExactMatchOnly(t *testing.T) {
	subs := []rbacv1.Subject{
		{Kind: rbacv1.ServiceAccountKind, Namespace: "other", Name: "headlamp"},
		{Kind: "User", Namespace: "headlamp", Name: "headlamp"},
	}
	if subjectsInclude(subs, "headlamp", "headlamp") {
		t.Fatal("wrong-namespace or wrong-kind subjects must not match")
	}
}

func TestNoCodePathReferencesThirdPartyHeadlampImage(t *testing.T) {
	o, err := Options{}.Normalize()
	if err != nil {
		t.Fatalf("zero-value Options must normalize: %v", err)
	}
	if o.Image != "" {
		t.Fatalf("expected empty Image default, got %q", o.Image)
	}
	raw, err := RenderJSON(Options{})
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if strings.Contains(string(raw), "ghcr.io/headlamp-k8s") {
		t.Fatal("RenderJSON output must not reference ghcr.io/headlamp-k8s")
	}
}
