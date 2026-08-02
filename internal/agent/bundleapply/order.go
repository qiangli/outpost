package bundleapply

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// applyRank assigns a stable ordering priority to a Kubernetes kind so
// that prerequisites are applied before the objects that depend on them.
// The hard requirement is Namespace before any namespaced object; the
// wider ordering mirrors the well-known Helm install order so RBAC,
// config, and services exist before the workloads that reference them.
//
// Lower rank applies first. Unknown kinds land in the middle default
// band (after config/RBAC, before workloads) — a conservative spot that
// keeps custom resources from racing their operators without assuming
// anything about them.
var applyRank = map[string]int{
	// Cluster-scoped prerequisites first — Namespace MUST precede any
	// namespaced object.
	"Namespace":                0,
	"CustomResourceDefinition": 1,
	"PriorityClass":            2,
	"StorageClass":             3,
	"RuntimeClass":             4,
	"PodSecurityPolicy":        5,
	"ClusterRole":              6,
	"ClusterRoleList":          6,
	"ClusterRoleBinding":       7,
	"ClusterRoleBindingList":   7,
	"ServiceAccount":           8,
	"Role":                     9,
	"RoleBinding":              10,
	"Secret":                   11,
	"SecretList":               11,
	"ConfigMap":                12,
	"LimitRange":               13,
	"ResourceQuota":            14,
	"NetworkPolicy":            15,
	"PersistentVolume":         16,
	"PersistentVolumeClaim":    17,
	"Service":                  18,
	"Endpoints":                19,
	// Workloads last — they consume everything above.
	"Pod":                            40,
	"DaemonSet":                      41,
	"ReplicaSet":                     42,
	"ReplicationController":          43,
	"Deployment":                     44,
	"StatefulSet":                    45,
	"Job":                            46,
	"CronJob":                        47,
	"HorizontalPodAutoscaler":        48,
	"Ingress":                        49,
	"IngressClass":                   49,
	"APIService":                     50,
	"MutatingWebhookConfiguration":   51,
	"ValidatingWebhookConfiguration": 52,
}

// defaultApplyRank is used for any kind not in applyRank — after
// config/RBAC (so cluster-scoped support objects come first) and before
// the workload band (so a CR is applied before a Deployment that might
// consume it).
const defaultApplyRank = 30

func rankOf(kind string) int {
	if r, ok := applyRank[kind]; ok {
		return r
	}
	return defaultApplyRank
}

// sortByApplyRank sorts objects in place by apply rank, breaking ties
// deterministically by (namespace, name, kind) so two runs over the same
// bundle produce the identical apply sequence.
func sortByApplyRank(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		ri, rj := rankOf(objs[i].GetKind()), rankOf(objs[j].GetKind())
		if ri != rj {
			return ri < rj
		}
		if a, b := objs[i].GetNamespace(), objs[j].GetNamespace(); a != b {
			return a < b
		}
		if a, b := objs[i].GetName(), objs[j].GetName(); a != b {
			return a < b
		}
		return objs[i].GetKind() < objs[j].GetKind()
	})
}
