package federation

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// EnsureBootstrapDiscovery fills the operator install's missing pull-mode
// bootstrap resources. These are the Karmada v1.19 chart's discovery/RBAC
// contract. The operator creates the API and admin Secret but not these
// resources; without them karmadactl register retries discovery for 5 minutes.
func EnsureBootstrapDiscovery(ctx context.Context, kubeconfig string, port int) error {
	raw, err := os.ReadFile(kubeconfig)
	if err != nil {
		return err
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return err
	}
	rest, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return err
	}
	return ensureBootstrapWithClient(ctx, cs, cfg, port)
}

func ensureBootstrapWithClient(ctx context.Context, cs kubernetes.Interface, admin *clientcmdapi.Config, port int) error {
	context := admin.Contexts[admin.CurrentContext]
	if context == nil || admin.Clusters[context.Cluster] == nil {
		return fmt.Errorf("Karmada admin kubeconfig has no current cluster")
	}
	ca := admin.Clusters[context.Cluster].CertificateAuthorityData
	if len(ca) == 0 {
		return fmt.Errorf("Karmada admin kubeconfig has no CA data")
	}
	public := clientcmdapi.NewConfig()
	public.Clusters["karmada"] = &clientcmdapi.Cluster{Server: LoopbackServer(port), CertificateAuthorityData: ca}
	public.Contexts["karmada"] = &clientcmdapi.Context{Cluster: "karmada"}
	public.CurrentContext = "karmada"
	data, err := clientcmd.Write(*public)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: metav1.NamespacePublic}, Data: map[string]string{"kubeconfig": string(data)}}
	if _, err := cs.CoreV1().ConfigMaps(metav1.NamespacePublic).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create cluster-info: %w", err)
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "system:karmada:bootstrap-signer-clusterinfo", Namespace: metav1.NamespacePublic}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"cluster-info"}, Verbs: []string{"get"}}}}
	if _, err := cs.RbacV1().Roles(metav1.NamespacePublic).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create bootstrap discovery role: %w", err)
	}
	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: metav1.NamespacePublic}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "system:anonymous"}}}
	if _, err := cs.RbacV1().RoleBindings(metav1.NamespacePublic).Create(ctx, roleBinding, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create bootstrap discovery binding: %w", err)
	}
	group := rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "system:bootstrappers:karmada:default-cluster-token"}
	if err := createBootstrapBinding(ctx, cs, "system:karmada:agent-bootstrap", "system:node-bootstrapper", group); err != nil {
		return err
	}
	csrRule := func(subresource string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests/" + subresource}, Verbs: []string{"create"}}}
	}
	if err := createBootstrapRole(ctx, cs, "system:karmada:certificatesigningrequest:autoapprover", csrRule("clusteragent")); err != nil {
		return err
	}
	if err := createBootstrapBinding(ctx, cs, "system:karmada:agent-autoapprove-bootstrap", "system:karmada:certificatesigningrequest:autoapprover", group); err != nil {
		return err
	}
	if err := createBootstrapRole(ctx, cs, "system:karmada:certificatesigningrequest:selfautoapprover", csrRule("selfclusteragent")); err != nil {
		return err
	}
	if err := createBootstrapBinding(ctx, cs, "system:karmada:agent-autoapprove-certificate-rotation", "system:karmada:certificatesigningrequest:selfautoapprover", rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "system:karmada:agents"}); err != nil {
		return err
	}
	if err := createBootstrapRole(ctx, cs, "system:karmada:agent-rbac-generator", []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}); err != nil {
		return err
	}
	return createBootstrapBinding(ctx, cs, "system:karmada:agent-rbac-generator", "system:karmada:agent-rbac-generator", rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: "User", Name: "system:karmada:agent:rbac-generator"})
}

func createBootstrapRole(ctx context.Context, cs kubernetes.Interface, name string, rules []rbacv1.PolicyRule) error {
	_, err := cs.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create bootstrap role %s: %w", name, err)
	}
	return nil
}

func createBootstrapBinding(ctx context.Context, cs kubernetes.Interface, name, role string, subject rbacv1.Subject) error {
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role}, Subjects: []rbacv1.Subject{subject}}
	_, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create bootstrap binding %s: %w", name, err)
	}
	return nil
}
