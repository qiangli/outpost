package headlamp

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Posture is the audited state of a Headlamp deployment. It is
// FAIL-CLOSED at the API layer: Inspect returns an error — not a
// Posture — whenever any query it needs could not be answered, so a
// flaky apiserver can never read as "secure".
type Posture struct {
	DeploymentFound bool
	ReadyReplicas   int32
	ServiceFound    bool
	ServiceType     corev1.ServiceType

	// Violations lists every observed deviation from the security
	// model: an exposed Service (NodePort/LoadBalancer/externalIPs),
	// host namespaces or hostPorts, a privileged container, a pod not
	// running as the zero-grant SA, or ANY RBAC binding attached to
	// the pod SA (it must have none) or unexpected binding on the
	// viewer SA.
	Violations []string
}

// Running reports whether the deployment exists and has at least one
// ready replica.
func (p *Posture) Running() bool {
	return p != nil && p.DeploymentFound && p.ReadyReplicas >= 1
}

// Secure reports whether the audited surface matches the security
// model: both objects present and zero violations.
func (p *Posture) Secure() bool {
	return p != nil && p.DeploymentFound && p.ServiceFound && len(p.Violations) == 0
}

// Inspect audits the live cluster state against the security model.
// Missing deployment/service is reported via the Found fields (that is
// an answer, not an error); any API failure is an error.
func Inspect(ctx context.Context, cs kubernetes.Interface, o Options) (*Posture, error) {
	o, err := o.Normalize()
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return nil, fmt.Errorf("headlamp: nil kubernetes client")
	}
	p := &Posture{}

	dep, err := cs.AppsV1().Deployments(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("headlamp: inspect deployment: %w", err)
	default:
		p.DeploymentFound = true
		p.ReadyReplicas = dep.Status.ReadyReplicas
		p.Violations = append(p.Violations,
			podSpecViolations("deployment template", &dep.Spec.Template.Spec)...)
		if sa := dep.Spec.Template.Spec.ServiceAccountName; sa != o.Name {
			p.Violations = append(p.Violations,
				fmt.Sprintf("deployment template runs as serviceaccount %q, want the zero-grant %q", sa, o.Name))
		}
	}

	svc, err := cs.CoreV1().Services(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("headlamp: inspect service: %w", err)
	default:
		p.ServiceFound = true
		p.ServiceType = svc.Spec.Type
		if svc.Spec.Type != corev1.ServiceTypeClusterIP {
			p.Violations = append(p.Violations,
				fmt.Sprintf("service type is %s, want ClusterIP", svc.Spec.Type))
		}
		for _, port := range svc.Spec.Ports {
			if port.NodePort != 0 {
				p.Violations = append(p.Violations,
					fmt.Sprintf("service port %s exposes nodePort %d", port.Name, port.NodePort))
			}
		}
		if len(svc.Spec.ExternalIPs) > 0 {
			p.Violations = append(p.Violations,
				fmt.Sprintf("service carries externalIPs %v", svc.Spec.ExternalIPs))
		}
	}

	pods, err := cs.CoreV1().Pods(o.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: nameLabel + "=" + o.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("headlamp: inspect pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		p.Violations = append(p.Violations,
			podSpecViolations("pod "+pod.Name, &pod.Spec)...)
		if sa := pod.Spec.ServiceAccountName; sa != o.Name {
			p.Violations = append(p.Violations,
				fmt.Sprintf("pod %s runs as serviceaccount %q, want the zero-grant %q", pod.Name, sa, o.Name))
		}
	}

	rbacViolations, err := inspectRBAC(ctx, cs, o)
	if err != nil {
		return nil, err
	}
	p.Violations = append(p.Violations, rbacViolations...)
	return p, nil
}

// inspectRBAC enforces the grant boundary:
//   - the pod SA (<ns>/<name>) must appear in NO binding, cluster or
//     namespaced — its powerlessness is the auth boundary;
//   - the viewer SA may appear only in the two package-owned
//     ClusterRoleBindings (view + nodeview); anything else is drift.
func inspectRBAC(ctx context.Context, cs kubernetes.Interface, o Options) ([]string, error) {
	expectedViewer := map[string]string{
		viewerBindingView:  "view",
		viewerBindingNodes: NodeViewClusterRole,
	}
	var violations []string

	crbs, err := cs.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("headlamp: inspect clusterrolebindings: %w", err)
	}
	for i := range crbs.Items {
		b := &crbs.Items[i]
		if !bindingReferences(b.Subjects, o.Namespace, o.Name, ViewerServiceAccount) {
			continue
		}
		if subjectsInclude(b.Subjects, o.Namespace, o.Name) {
			violations = append(violations,
				fmt.Sprintf("clusterrolebinding %s grants %s/%s to %s %s — the pod SA must have zero grants",
					b.Name, o.Namespace, o.Name, b.RoleRef.Kind, b.RoleRef.Name))
		}
		if subjectsInclude(b.Subjects, o.Namespace, ViewerServiceAccount) {
			if want, ok := expectedViewer[b.Name]; !ok || b.RoleRef.Kind != "ClusterRole" || b.RoleRef.Name != want {
				violations = append(violations,
					fmt.Sprintf("clusterrolebinding %s grants %s %s to the viewer SA — only the outpost view+nodeview bindings are expected",
						b.Name, b.RoleRef.Kind, b.RoleRef.Name))
			}
		}
	}

	rbs, err := cs.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("headlamp: inspect rolebindings: %w", err)
	}
	for i := range rbs.Items {
		b := &rbs.Items[i]
		if subjectsInclude(b.Subjects, o.Namespace, o.Name) {
			violations = append(violations,
				fmt.Sprintf("rolebinding %s/%s grants %s %s to the pod SA — it must have zero grants",
					b.Namespace, b.Name, b.RoleRef.Kind, b.RoleRef.Name))
		}
		if subjectsInclude(b.Subjects, o.Namespace, ViewerServiceAccount) {
			violations = append(violations,
				fmt.Sprintf("rolebinding %s/%s grants %s %s to the viewer SA — only the outpost view+nodeview clusterrolebindings are expected",
					b.Namespace, b.Name, b.RoleRef.Kind, b.RoleRef.Name))
		}
	}
	return violations, nil
}

func subjectsInclude(subjects []rbacv1.Subject, ns, name string) bool {
	for _, s := range subjects {
		if s.Kind == rbacv1.ServiceAccountKind && s.Namespace == ns && s.Name == name {
			return true
		}
	}
	return false
}

func bindingReferences(subjects []rbacv1.Subject, ns string, names ...string) bool {
	for _, n := range names {
		if subjectsInclude(subjects, ns, n) {
			return true
		}
	}
	return false
}
