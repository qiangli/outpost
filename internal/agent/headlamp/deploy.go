package headlamp

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Deploy creates or converges the Headlamp deployment on the plane the
// client points at. It is idempotent and self-repairing: a drifted
// Service type is forced back to ClusterIP, and when DisableViewerRBAC
// is set any previously created viewer artifacts are removed. It never
// binds anything to the pod ServiceAccount.
func Deploy(ctx context.Context, cs kubernetes.Interface, o Options) error {
	o, err := o.Normalize()
	if err != nil {
		return err
	}
	if cs == nil {
		return fmt.Errorf("headlamp: nil kubernetes client")
	}

	ns := o.namespaceObject()
	if existing, err := cs.CoreV1().Namespaces().Get(ctx, ns.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("headlamp: create namespace %s: %w", ns.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("headlamp: get namespace %s: %w", ns.Name, err)
	} else if existing.Labels[managedByLabel] != managedByValue {
		// A pre-existing namespace we did not create: refuse rather
		// than adopt — Remove would otherwise delete somebody else's
		// namespace later.
		return fmt.Errorf("headlamp: namespace %s exists but is not managed by outpost; pick another Options.Namespace", ns.Name)
	}

	sa := o.podServiceAccount()
	if _, err := cs.CoreV1().ServiceAccounts(o.Namespace).Get(ctx, sa.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.CoreV1().ServiceAccounts(o.Namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("headlamp: create serviceaccount %s: %w", sa.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("headlamp: get serviceaccount %s: %w", sa.Name, err)
	}

	if o.DisableViewerRBAC {
		if err := removeViewerRBAC(ctx, cs, o); err != nil {
			return err
		}
	} else if err := deployViewerRBAC(ctx, cs, o); err != nil {
		return err
	}

	dep := o.deployment()
	if existing, err := cs.AppsV1().Deployments(o.Namespace).Get(ctx, dep.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.AppsV1().Deployments(o.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("headlamp: create deployment %s: %w", dep.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("headlamp: get deployment %s: %w", dep.Name, err)
	} else {
		dep.ResourceVersion = existing.ResourceVersion
		if _, err := cs.AppsV1().Deployments(o.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("headlamp: update deployment %s: %w", dep.Name, err)
		}
	}

	svc := o.service()
	if existing, err := cs.CoreV1().Services(o.Namespace).Get(ctx, svc.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.CoreV1().Services(o.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("headlamp: create service %s: %w", svc.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("headlamp: get service %s: %w", svc.Name, err)
	} else {
		// Converge type back to ClusterIP but preserve every
		// immutable or server-defaulted field — a naive update
		// that strips ClusterIPs/IPFamilies/IPFamilyPolicy
		// fails against a real apiserver.
		svc.ResourceVersion = existing.ResourceVersion
		svc.Spec.ClusterIP = existing.Spec.ClusterIP
		svc.Spec.ClusterIPs = existing.Spec.ClusterIPs
		svc.Spec.IPFamilies = existing.Spec.IPFamilies
		svc.Spec.IPFamilyPolicy = existing.Spec.IPFamilyPolicy
		if _, err := cs.CoreV1().Services(o.Namespace).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("headlamp: update service %s: %w", svc.Name, err)
		}
	}
	return nil
}

func deployViewerRBAC(ctx context.Context, cs kubernetes.Interface, o Options) error {
	vsa := o.viewerServiceAccount()
	if _, err := cs.CoreV1().ServiceAccounts(o.Namespace).Get(ctx, vsa.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.CoreV1().ServiceAccounts(o.Namespace).Create(ctx, vsa, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("headlamp: create serviceaccount %s: %w", vsa.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("headlamp: get serviceaccount %s: %w", vsa.Name, err)
	}

	role := o.nodeViewClusterRole()
	if existing, err := cs.RbacV1().ClusterRoles().Get(ctx, role.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("headlamp: create clusterrole %s: %w", role.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("headlamp: get clusterrole %s: %w", role.Name, err)
	} else {
		// Converge the rules: a widened nodeview role is a posture
		// drift Deploy must repair, not preserve.
		role.ResourceVersion = existing.ResourceVersion
		if _, err := cs.RbacV1().ClusterRoles().Update(ctx, role, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("headlamp: update clusterrole %s: %w", role.Name, err)
		}
	}

	for binding, roleName := range map[string]string{
		viewerBindingView:  "view",
		viewerBindingNodes: NodeViewClusterRole,
	} {
		crb := o.viewerBinding(binding, roleName)
		if existing, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, binding, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			if _, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("headlamp: create clusterrolebinding %s: %w", binding, err)
			}
		} else if err != nil {
			return fmt.Errorf("headlamp: get clusterrolebinding %s: %w", binding, err)
		} else {
			// roleRef is immutable in the real API; if it drifted the
			// binding must be replaced, not updated.
			if existing.RoleRef != crb.RoleRef {
				if err := cs.RbacV1().ClusterRoleBindings().Delete(ctx, binding, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("headlamp: replace clusterrolebinding %s: %w", binding, err)
				}
				if _, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil {
					return fmt.Errorf("headlamp: recreate clusterrolebinding %s: %w", binding, err)
				}
				continue
			}
			crb.ResourceVersion = existing.ResourceVersion
			if _, err := cs.RbacV1().ClusterRoleBindings().Update(ctx, crb, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("headlamp: update clusterrolebinding %s: %w", binding, err)
			}
		}
	}
	return nil
}

func removeViewerRBAC(ctx context.Context, cs kubernetes.Interface, o Options) error {
	for _, binding := range []string{viewerBindingView, viewerBindingNodes} {
		if err := cs.RbacV1().ClusterRoleBindings().Delete(ctx, binding, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("headlamp: delete clusterrolebinding %s: %w", binding, err)
		}
	}
	if err := cs.RbacV1().ClusterRoles().Delete(ctx, NodeViewClusterRole, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("headlamp: delete clusterrole %s: %w", NodeViewClusterRole, err)
	}
	if err := cs.CoreV1().ServiceAccounts(o.Namespace).Delete(ctx, ViewerServiceAccount, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("headlamp: delete serviceaccount %s: %w", ViewerServiceAccount, err)
	}
	return nil
}

// Remove deletes everything Deploy created, viewer RBAC included. The
// namespace itself is deleted only when it carries the outpost
// managed-by label — a namespace this package did not create is never
// destroyed. NotFound at any step is fine (idempotent).
func Remove(ctx context.Context, cs kubernetes.Interface, o Options) error {
	o, err := o.Normalize()
	if err != nil {
		return err
	}
	if cs == nil {
		return fmt.Errorf("headlamp: nil kubernetes client")
	}
	if err := removeViewerRBAC(ctx, cs, o); err != nil {
		return err
	}
	if err := cs.CoreV1().Services(o.Namespace).Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("headlamp: delete service %s: %w", o.Name, err)
	}
	if err := cs.AppsV1().Deployments(o.Namespace).Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("headlamp: delete deployment %s: %w", o.Name, err)
	}
	if err := cs.CoreV1().ServiceAccounts(o.Namespace).Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("headlamp: delete serviceaccount %s: %w", o.Name, err)
	}

	ns, err := cs.CoreV1().Namespaces().Get(ctx, o.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("headlamp: get namespace %s: %w", o.Namespace, err)
	}
	if ns.Labels[managedByLabel] != managedByValue {
		// Not ours: leave it standing.
		return nil
	}
	if err := cs.CoreV1().Namespaces().Delete(ctx, o.Namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("headlamp: delete namespace %s: %w", o.Namespace, err)
	}
	return nil
}

// podIsHardened reports the hostNetwork / hostPort / privileged
// violations of one pod spec, prefixed with subject for attribution.
func podSpecViolations(subject string, spec *corev1.PodSpec) []string {
	var v []string
	if spec.HostNetwork {
		v = append(v, subject+" uses hostNetwork")
	}
	if spec.HostPID || spec.HostIPC {
		v = append(v, subject+" uses a host PID/IPC namespace")
	}
	containers := make([]corev1.Container, 0, len(spec.Containers)+len(spec.InitContainers))
	containers = append(containers, spec.InitContainers...)
	containers = append(containers, spec.Containers...)
	for _, c := range containers {
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				v = append(v, fmt.Sprintf("%s container %s binds hostPort %d", subject, c.Name, p.HostPort))
			}
		}
		if sc := c.SecurityContext; sc != nil && sc.Privileged != nil && *sc.Privileged {
			v = append(v, fmt.Sprintf("%s container %s is privileged", subject, c.Name))
		}
	}
	return v
}
