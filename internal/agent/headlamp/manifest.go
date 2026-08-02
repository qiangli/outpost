package headlamp

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// labels every managed object carries. name is the selector key the
// acceptance harness and the posture checks both key on.
func (o Options) labels() map[string]string {
	return map[string]string{
		nameLabel:      o.Name,
		partOfLabel:    partOfValue,
		managedByLabel: managedByValue,
	}
}

func (o Options) objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: o.Namespace,
		Labels:    o.labels(),
	}
}

// namespaceObject is the dedicated namespace. Dedicated + labelled so
// Remove can verify ownership before deleting it.
func (o Options) namespaceObject() *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: o.Namespace, Labels: o.labels()},
	}
}

// podServiceAccount is the SA the Headlamp pod runs as. It is bound to
// NOTHING — that absence is the core of the auth boundary (layer 1 of
// the package doc) and Inspect flags any binding that appears on it.
func (o Options) podServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: o.objectMeta(o.Name),
	}
}

// viewerServiceAccount is the optional token-minting SA. No pod mounts
// it; it exists so the operator can mint a read-only token to paste at
// Headlamp's login screen.
func (o Options) viewerServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: o.objectMeta(ViewerServiceAccount),
	}
}

// nodeViewClusterRole is the one custom grant: read-only nodes. The
// built-in "view" aggregate covers namespaced resources only; nodes are
// cluster-scoped, and a control-plane operating UI that cannot list
// nodes is pointless. get/list/watch, nothing else.
func (o Options) nodeViewClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: NodeViewClusterRole, Labels: o.labels()},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
}

func (o Options) viewerBinding(bindingName, roleName string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Labels: o.labels()},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Namespace: o.Namespace,
			Name:      ViewerServiceAccount,
		}},
	}
}

// deployment is the hardened Headlamp pod shape: token-login mode
// (-in-cluster with a powerless SA), non-root, no privilege
// escalation, all capabilities dropped, default seccomp, no
// hostNetwork/hostPort anywhere.
func (o Options) deployment() *appsv1.Deployment {
	replicas := int32(1)
	runAsNonRoot := true
	noEscalation := false
	automount := true // needed only for in-cluster apiserver addr + CA; the token itself has zero grants
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: o.objectMeta(o.Name),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: o.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: o.labels()},
				Spec: corev1.PodSpec{
					ServiceAccountName:           o.Name,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  o.Name,
						Image: o.Image,
						Args:  []string{"-in-cluster"},
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: containerPort,
							Protocol:      corev1.ProtocolTCP,
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noEscalation,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt32(containerPort),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					}},
				},
			},
		},
	}
}

// service is ClusterIP-only by construction. Deploy resets a drifted
// type back to ClusterIP; Inspect flags it.
func (o Options) service() *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: o.objectMeta(o.Name),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{nameLabel: o.Name},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       ServicePort,
				TargetPort: intstr.FromInt32(containerPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// RenderJSON renders the full manifest as a v1 List in JSON — kubectl
// applies JSON exactly like YAML (`kubectl apply -f headlamp.json`),
// and JSON needs no extra dependency. Output is deterministic for a
// given Options value. The viewer RBAC objects are included unless
// DisableViewerRBAC is set.
func RenderJSON(o Options) ([]byte, error) {
	o, err := o.Normalize()
	if err != nil {
		return nil, err
	}
	items := []any{
		o.namespaceObject(),
		o.podServiceAccount(),
	}
	if !o.DisableViewerRBAC {
		items = append(items,
			o.viewerServiceAccount(),
			o.nodeViewClusterRole(),
			o.viewerBinding(viewerBindingView, "view"),
			o.viewerBinding(viewerBindingNodes, NodeViewClusterRole),
		)
	}
	items = append(items, o.deployment(), o.service())
	list := struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Items      []any  `json:"items"`
	}{APIVersion: "v1", Kind: "List", Items: items}
	out, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("headlamp: render manifest: %w", err)
	}
	return append(out, '\n'), nil
}
