package headlamp

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Deploy fails closed before touching the cluster because headlamp is disabled
// by the service supply-chain policy (no bashy-signed build yet; blocked on
// upstream fixes for GO-2026-5064/5338/5622/5932). Options.Image overrides do
// not bypass this refusal.
func Deploy(ctx context.Context, cs kubernetes.Interface, o Options) error {
	return ErrDisabled
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
