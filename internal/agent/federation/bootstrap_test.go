package federation

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
)

func TestEnsureBootstrapDiscoveryForOperatorInstall(t *testing.T) {
	ctx := context.Background()
	ca, _ := testCA(t)
	admin, err := clientcmd.Load(adminKubeconfig(t, ca, "https://127.0.0.1:15443"))
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset()
	for range 2 {
		if err := ensureBootstrapWithClient(ctx, cs, admin, 15443); err != nil {
			t.Fatal(err)
		}
	}
	cm, err := cs.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	public, err := clientcmd.Load([]byte(cm.Data["kubeconfig"]))
	if err != nil {
		t.Fatal(err)
	}
	if len(public.AuthInfos) != 0 || public.Clusters["karmada"].Server != "https://127.0.0.1:15443" || len(public.Clusters["karmada"].CertificateAuthorityData) == 0 {
		t.Fatal("cluster-info must publish only the pinned CA and discovery endpoint")
	}
	for _, name := range []string{"system:karmada:agent-bootstrap", "system:karmada:agent-autoapprove-bootstrap", "system:karmada:agent-autoapprove-certificate-rotation", "system:karmada:agent-rbac-generator"} {
		if _, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cs.RbacV1().RoleBindings(metav1.NamespacePublic).Get(ctx, "system:karmada:bootstrap-signer-clusterinfo", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}
