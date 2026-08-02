package bundleapply

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readyState is the outcome of evaluating one object's readiness.
type readyState struct {
	// ready is true only when the object has affirmatively reported Ready.
	ready bool
	// terminal, when non-nil, means the object has reached a failed state
	// that waiting cannot fix (e.g. a Pod in phase Failed, or a workload
	// declaring zero desired replicas without the explicit opt-in). The
	// caller aborts the wait immediately rather than burning the timeout.
	terminal error
	// reason is a short human string for logs/errors.
	reason string
}

// evalReadiness decides whether a live object is Ready.
//
// The evidence invariant governs the defaults: readiness must be
// AFFIRMATIVELY observed, and "rolled out" is the bar — not merely
// "some replicas ready". Concretely:
//
//   - The controller must have observed the CURRENT spec
//     (status.observedGeneration >= metadata.generation) before any
//     counter is trusted; stale status describes the old spec.
//   - Deployments / StatefulSets require UPDATED replicas, not just ready
//     ones — ready replicas from the old ReplicaSet are not a rollout.
//   - StatefulSets additionally require currentRevision == updateRevision.
//   - A workload declaring spec.replicas: 0 is REJECTED as trivially
//     ready unless allowZero (the caller's explicit opt-in) is set. Zero
//     desired satisfying "all replicas ready" is a green built on the
//     absence of evidence.
//
// Kinds with no rollout concept (Namespace, ConfigMap, Secret, Service,
// ServiceAccount, RBAC, …) are Ready as soon as they exist, because their
// creation IS the evidence. CustomResourceDefinitions are Ready only once
// the apiserver reports the Established condition.
func evalReadiness(obj *unstructured.Unstructured, allowZero bool) readyState {
	switch obj.GetKind() {
	case "Deployment":
		return evalDeployment(obj, allowZero)
	case "StatefulSet":
		return evalStatefulSet(obj, allowZero)
	case "ReplicaSet", "ReplicationController":
		return evalReplicaSet(obj, allowZero)
	case "DaemonSet":
		return evalDaemonSet(obj, allowZero)
	case "Pod":
		return evalPod(obj)
	case "Job":
		return evalJob(obj)
	case "CustomResourceDefinition":
		return evalCRD(obj)
	case "HelmChart":
		return evalHelmChart(obj)
	default:
		// Existence is the evidence for objects without a rollout.
		return readyState{ready: true, reason: obj.GetKind() + " exists"}
	}
}

// observedCurrent reports whether the controller has observed the latest
// spec (status.observedGeneration >= metadata.generation). Until it has,
// the replica counters describe the OLD spec and must not be trusted.
func observedCurrent(obj *unstructured.Unstructured) bool {
	gen, found, _ := unstructured.NestedInt64(obj.Object, "metadata", "generation")
	if !found {
		// No generation stamped yet — cannot claim the status is current.
		return false
	}
	observed, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if !found {
		return false
	}
	return observed >= gen
}

// desiredReplicas reads spec.replicas, defaulting to 1 when unset
// (matching the apiserver's defaulting for Deployment / StatefulSet /
// ReplicaSet). The bool reports whether zero was EXPLICITLY declared.
func desiredReplicas(obj *unstructured.Unstructured) (desired int64, explicitZero bool) {
	desired = 1
	if r, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); found {
		desired = r
		explicitZero = r == 0
	}
	return desired, explicitZero
}

// zeroDesired handles the spec.replicas: 0 case shared by every replica
// workload. Without the opt-in it is TERMINAL — waiting cannot make a
// zero-desired workload produce evidence of anything running, so burning
// the timeout would only delay the same answer.
func zeroDesired(obj *unstructured.Unstructured, kind string, allowZero bool) readyState {
	if !allowZero {
		return readyState{terminal: fmt.Errorf("%w: %s %s/%s", ErrZeroDesiredWorkload, kind, obj.GetNamespace(), obj.GetName())}
	}
	if !observedCurrent(obj) {
		return readyState{reason: kind + ": controller has not observed the latest spec yet"}
	}
	// Scaled-to-zero on purpose: the opt-in is the operator's explicit
	// evidence, and total replicas must actually have drained to zero.
	if total, _, _ := unstructured.NestedInt64(obj.Object, "status", "replicas"); total > 0 {
		return readyState{reason: fmt.Sprintf("%s: scaling down, %d replicas remain", kind, total)}
	}
	return readyState{ready: true, reason: kind + ": scaled to zero (explicit opt-in)"}
}

// evalDeployment: observedGeneration current, and updated + ready +
// available replicas all at desired. Updated is the load-bearing one —
// ready replicas belonging to the OLD ReplicaSet are not a rollout.
func evalDeployment(obj *unstructured.Unstructured, allowZero bool) readyState {
	desired, explicitZero := desiredReplicas(obj)
	if explicitZero {
		return zeroDesired(obj, "Deployment", allowZero)
	}
	if !observedCurrent(obj) {
		return readyState{reason: "Deployment: controller has not observed the latest spec yet"}
	}
	updated, _, _ := unstructured.NestedInt64(obj.Object, "status", "updatedReplicas")
	if updated < desired {
		return readyState{reason: fmt.Sprintf("Deployment: %d/%d replicas updated to the new template", updated, desired)}
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	if ready < desired {
		return readyState{reason: fmt.Sprintf("Deployment: %d/%d replicas ready", ready, desired)}
	}
	avail, _, _ := unstructured.NestedInt64(obj.Object, "status", "availableReplicas")
	if avail < desired {
		return readyState{reason: fmt.Sprintf("Deployment: %d/%d replicas available", avail, desired)}
	}
	return readyState{ready: true, reason: fmt.Sprintf("Deployment: %d/%d updated+ready+available", ready, desired)}
}

// evalStatefulSet: observedGeneration current, updated + ready replicas at
// desired, AND status.currentRevision == status.updateRevision. The
// revision equality is what proves the rollout completed — a StatefulSet
// mid-update reports ready replicas from the old revision. Missing
// revision fields are NOT ready (absence is not evidence).
func evalStatefulSet(obj *unstructured.Unstructured, allowZero bool) readyState {
	desired, explicitZero := desiredReplicas(obj)
	if explicitZero {
		return zeroDesired(obj, "StatefulSet", allowZero)
	}
	if !observedCurrent(obj) {
		return readyState{reason: "StatefulSet: controller has not observed the latest spec yet"}
	}
	updated, _, _ := unstructured.NestedInt64(obj.Object, "status", "updatedReplicas")
	if updated < desired {
		return readyState{reason: fmt.Sprintf("StatefulSet: %d/%d replicas updated to the new revision", updated, desired)}
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	if ready < desired {
		return readyState{reason: fmt.Sprintf("StatefulSet: %d/%d replicas ready", ready, desired)}
	}
	current, curFound, _ := unstructured.NestedString(obj.Object, "status", "currentRevision")
	update, updFound, _ := unstructured.NestedString(obj.Object, "status", "updateRevision")
	if !curFound || !updFound || current == "" || update == "" {
		return readyState{reason: "StatefulSet: revision status not reported yet"}
	}
	if current != update {
		return readyState{reason: fmt.Sprintf("StatefulSet: rollout in progress (currentRevision %s != updateRevision %s)", current, update)}
	}
	return readyState{ready: true, reason: fmt.Sprintf("StatefulSet: %d/%d ready at revision %s", ready, desired, update)}
}

// evalReplicaSet handles ReplicaSet / ReplicationController: observed
// generation current, ready + available at desired. (These have no
// updated-replica or revision concept — a rolling update happens one
// level up, at the Deployment.)
func evalReplicaSet(obj *unstructured.Unstructured, allowZero bool) readyState {
	kind := obj.GetKind()
	desired, explicitZero := desiredReplicas(obj)
	if explicitZero {
		return zeroDesired(obj, kind, allowZero)
	}
	if !observedCurrent(obj) {
		return readyState{reason: kind + ": controller has not observed the latest spec yet"}
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	if ready < desired {
		return readyState{reason: fmt.Sprintf("%s: %d/%d replicas ready", kind, ready, desired)}
	}
	avail, _, _ := unstructured.NestedInt64(obj.Object, "status", "availableReplicas")
	if avail < desired {
		return readyState{reason: fmt.Sprintf("%s: %d/%d replicas available", kind, avail, desired)}
	}
	return readyState{ready: true, reason: fmt.Sprintf("%s: %d/%d ready", kind, ready, desired)}
}

// evalDaemonSet handles DaemonSet readiness: numberReady and
// updatedNumberScheduled >= desiredNumberScheduled, with desired > 0
// required. A DaemonSet that matches zero nodes reports
// desiredNumberScheduled=0 — which is NOT evidence of a running workload.
// Unlike spec.replicas: 0 (a declared intent), zero scheduled nodes can
// be transient while nodes register, so without the opt-in we keep
// polling and let the bounded wait fail rather than aborting instantly.
func evalDaemonSet(obj *unstructured.Unstructured, allowZero bool) readyState {
	if !observedCurrent(obj) {
		return readyState{reason: "DaemonSet: controller has not observed the latest spec yet"}
	}
	desired, found, _ := unstructured.NestedInt64(obj.Object, "status", "desiredNumberScheduled")
	if !found {
		return readyState{reason: "DaemonSet: status.desiredNumberScheduled not reported yet"}
	}
	if desired == 0 {
		if allowZero {
			return readyState{ready: true, reason: "DaemonSet: 0 nodes scheduled (explicit opt-in)"}
		}
		return readyState{reason: "DaemonSet: 0 nodes scheduled (no node matches the selector — not proven running)"}
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberReady")
	updated, _, _ := unstructured.NestedInt64(obj.Object, "status", "updatedNumberScheduled")
	if ready < desired || updated < desired {
		return readyState{reason: fmt.Sprintf("DaemonSet: %d/%d ready, %d/%d updated", ready, desired, updated, desired)}
	}
	return readyState{ready: true, reason: fmt.Sprintf("DaemonSet: %d/%d ready", ready, desired)}
}

// evalPod handles a bare Pod. Running + Ready condition True is ready;
// Succeeded is ready (a completed one-shot); Failed is terminal.
func evalPod(obj *unstructured.Unstructured) readyState {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	switch phase {
	case "Succeeded":
		return readyState{ready: true, reason: "Pod: Succeeded"}
	case "Failed":
		return readyState{terminal: fmt.Errorf("pod %s/%s entered phase Failed", obj.GetNamespace(), obj.GetName())}
	case "Running":
		if conditionTrue(obj, "Ready") {
			return readyState{ready: true, reason: "Pod: Running and Ready"}
		}
		return readyState{reason: "Pod: Running but not Ready"}
	default:
		return readyState{reason: fmt.Sprintf("Pod: phase %q", phase)}
	}
}

// evalJob handles a Job: succeeded >= completions (default 1) is ready.
// Job status condition Failed=True is a terminal failure.
func evalJob(obj *unstructured.Unstructured) readyState {
	if conditionTrue(obj, "Failed") {
		return readyState{terminal: fmt.Errorf("job %s/%s entered condition Failed", obj.GetNamespace(), obj.GetName())}
	}
	completions := int64(1)
	if c, found, _ := unstructured.NestedInt64(obj.Object, "spec", "completions"); found {
		completions = c
	}
	succeeded, _, _ := unstructured.NestedInt64(obj.Object, "status", "succeeded")
	if succeeded >= completions {
		return readyState{ready: true, reason: fmt.Sprintf("Job: %d/%d succeeded", succeeded, completions)}
	}
	return readyState{reason: fmt.Sprintf("Job: %d/%d succeeded", succeeded, completions)}
}

// evalCRD: a CustomResourceDefinition is ready only once the apiserver
// affirmatively reports the Established condition True. Mere acceptance
// of the CRD object is not enough — applying a CR before Established
// races the discovery cache and fails intermittently.
func evalCRD(obj *unstructured.Unstructured) readyState {
	if conditionTrue(obj, "Established") {
		return readyState{ready: true, reason: "CustomResourceDefinition: Established"}
	}
	return readyState{reason: "CustomResourceDefinition: not Established yet"}
}

// evalHelmChart handles a k3s helm.cattle.io/v1 HelmChart custom
// resource (see internal/agent/appcatalog). The helm-controller reports
// two condition types on the object itself: JobCreated (the
// install/upgrade Job was established) and Failed (the job hit an
// error under a FailurePolicy of "abort"). There is no third condition
// confirming the underlying Job actually SUCCEEDED — that evidence lives
// on the Job the controller creates (named in status.jobName), which is
// outside this object and not tracked by this bundle. JobCreated+not
// Failed is therefore the strongest per-object evidence available; it
// proves the controller accepted and is driving the install, not that
// the Helm run has completed. This mirrors the same "no cluster is
// hardware-verified" caveat the rest of this package documents (see
// docs/peer-dks-bundle-apply.md) rather than a silent hidden guess.
func evalHelmChart(obj *unstructured.Unstructured) readyState {
	if conditionTrue(obj, "Failed") {
		return readyState{terminal: fmt.Errorf("HelmChart %s/%s reported condition Failed", obj.GetNamespace(), obj.GetName())}
	}
	if conditionTrue(obj, "JobCreated") {
		return readyState{ready: true, reason: "HelmChart: helm-controller created the install/upgrade job (JobCreated)"}
	}
	return readyState{reason: "HelmChart: waiting for the helm-controller to create the install/upgrade job"}
}

// conditionTrue reports whether the object carries the named status
// condition with status "True".
func conditionTrue(obj *unstructured.Unstructured, condType string) bool {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == condType && cm["status"] == "True" {
			return true
		}
	}
	return false
}
