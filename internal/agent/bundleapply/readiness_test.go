package bundleapply

import (
	"errors"
	"testing"
)

func TestReadinessNonWorkloadIsReadyOnExistence(t *testing.T) {
	for _, kind := range []string{"Namespace", "ConfigMap", "Secret", "Service", "ServiceAccount"} {
		o := newObj("v1", kind, "demo", "x")
		if st := evalReadiness(o, false); !st.ready {
			t.Fatalf("%s should be ready on existence, got %q", kind, st.reason)
		}
	}
}

func TestReadinessDeployment(t *testing.T) {
	o := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(o, int64(2), "spec", "replicas")
	setNested(o, int64(1), "metadata", "generation")

	// No status yet -> not ready (absence is not success).
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("deployment with no status must not be ready")
	}

	// observedGeneration stale -> not ready.
	setNested(o, int64(0), "status", "observedGeneration")
	setNested(o, int64(2), "status", "updatedReplicas")
	setNested(o, int64(2), "status", "readyReplicas")
	setNested(o, int64(2), "status", "availableReplicas")
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("deployment with stale observedGeneration must not be ready")
	}

	// Current generation but only 1/2 ready -> not ready.
	setNested(o, int64(1), "status", "observedGeneration")
	setNested(o, int64(1), "status", "readyReplicas")
	if st := evalReadiness(o, false); st.ready {
		t.Fatalf("1/2 ready must not be ready: %q", st.reason)
	}

	// Fully updated + ready + available.
	setNested(o, int64(2), "status", "readyReplicas")
	setNested(o, int64(2), "status", "availableReplicas")
	if st := evalReadiness(o, false); !st.ready {
		t.Fatalf("2/2 updated+ready+available must be ready: %q", st.reason)
	}
}

// The rollout requirement: a Deployment whose READY replicas all belong to
// the OLD ReplicaSet (updatedReplicas < desired) is NOT rolled out, no
// matter how green readyReplicas looks.
func TestReadinessDeploymentRequiresUpdatedReplicas(t *testing.T) {
	o := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(o, int64(2), "spec", "replicas")
	setNested(o, int64(3), "metadata", "generation")
	setNested(o, int64(3), "status", "observedGeneration")
	setNested(o, int64(2), "status", "readyReplicas")
	setNested(o, int64(2), "status", "availableReplicas")
	setNested(o, int64(1), "status", "updatedReplicas") // old RS still serving
	if st := evalReadiness(o, false); st.ready {
		t.Fatalf("ready-but-not-updated replicas must not count as rolled out: %q", st.reason)
	}
	setNested(o, int64(2), "status", "updatedReplicas")
	if st := evalReadiness(o, false); !st.ready {
		t.Fatalf("fully updated rollout must be ready: %q", st.reason)
	}
}

func TestReadinessDeploymentDefaultsReplicasToOne(t *testing.T) {
	o := newObj("apps/v1", "Deployment", "demo", "web")
	setNested(o, int64(1), "metadata", "generation")
	setNested(o, int64(1), "status", "observedGeneration")
	setNested(o, int64(1), "status", "updatedReplicas")
	setNested(o, int64(1), "status", "readyReplicas")
	setNested(o, int64(1), "status", "availableReplicas")
	if st := evalReadiness(o, false); !st.ready {
		t.Fatalf("deployment with implicit replicas=1 and 1 ready must be ready: %q", st.reason)
	}
}

// Zero desired replicas is an absence-of-evidence green: without the
// explicit opt-in it must be a TERMINAL failure (waiting cannot produce
// evidence), and with the opt-in it is ready only once drained.
func TestReadinessZeroDesiredIsRejectedWithoutOptIn(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "ReplicaSet"} {
		o := newObj("apps/v1", kind, "demo", "w")
		setNested(o, int64(0), "spec", "replicas")
		setNested(o, int64(1), "metadata", "generation")
		setNested(o, int64(1), "status", "observedGeneration")
		st := evalReadiness(o, false)
		if st.ready {
			t.Fatalf("%s with spec.replicas=0 must never be trivially ready", kind)
		}
		if st.terminal == nil || !errors.Is(st.terminal, ErrZeroDesiredWorkload) {
			t.Fatalf("%s with spec.replicas=0 must be terminal ErrZeroDesiredWorkload, got %+v", kind, st)
		}
	}
}

func TestReadinessZeroDesiredOptIn(t *testing.T) {
	o := newObj("apps/v1", "Deployment", "demo", "w")
	setNested(o, int64(0), "spec", "replicas")
	setNested(o, int64(1), "metadata", "generation")

	// Opt-in but controller has not observed the spec -> still waiting.
	if st := evalReadiness(o, true); st.ready || st.terminal != nil {
		if st.ready {
			t.Fatal("opt-in zero-desired must still wait for observedGeneration")
		}
	}
	setNested(o, int64(1), "status", "observedGeneration")
	// Old pods still draining -> not ready yet.
	setNested(o, int64(2), "status", "replicas")
	if st := evalReadiness(o, true); st.ready {
		t.Fatalf("opt-in zero-desired with pods remaining must not be ready: %q", st.reason)
	}
	setNested(o, int64(0), "status", "replicas")
	if st := evalReadiness(o, true); !st.ready {
		t.Fatalf("opt-in zero-desired, drained, observed -> ready; got %q", st.reason)
	}
}

func TestReadinessStatefulSetRequiresRevisionMatch(t *testing.T) {
	o := newObj("apps/v1", "StatefulSet", "demo", "db")
	setNested(o, int64(2), "spec", "replicas")
	setNested(o, int64(2), "metadata", "generation")
	setNested(o, int64(2), "status", "observedGeneration")
	setNested(o, int64(2), "status", "updatedReplicas")
	setNested(o, int64(2), "status", "readyReplicas")

	// Revision fields missing -> not ready (absence is not evidence).
	if st := evalReadiness(o, false); st.ready {
		t.Fatalf("statefulset without revision status must not be ready: %q", st.reason)
	}

	// Mid-rollout: current != update -> not ready even with counters green.
	setNested(o, "db-aaa", "status", "currentRevision")
	setNested(o, "db-bbb", "status", "updateRevision")
	if st := evalReadiness(o, false); st.ready {
		t.Fatalf("currentRevision != updateRevision must not be ready: %q", st.reason)
	}

	setNested(o, "db-bbb", "status", "currentRevision")
	if st := evalReadiness(o, false); !st.ready {
		t.Fatalf("revisions equal + counters at desired must be ready: %q", st.reason)
	}
}

func TestReadinessStatefulSetRequiresUpdatedReplicas(t *testing.T) {
	o := newObj("apps/v1", "StatefulSet", "demo", "db")
	setNested(o, int64(2), "spec", "replicas")
	setNested(o, int64(1), "metadata", "generation")
	setNested(o, int64(1), "status", "observedGeneration")
	setNested(o, int64(2), "status", "readyReplicas")
	setNested(o, int64(1), "status", "updatedReplicas")
	setNested(o, "r1", "status", "currentRevision")
	setNested(o, "r1", "status", "updateRevision")
	if st := evalReadiness(o, false); st.ready {
		t.Fatalf("1/2 updated must not be ready: %q", st.reason)
	}
}

func TestReadinessDaemonSetZeroDesiredIsNotReady(t *testing.T) {
	o := newObj("apps/v1", "DaemonSet", "demo", "ds")
	setNested(o, int64(1), "metadata", "generation")
	setNested(o, int64(1), "status", "observedGeneration")
	setNested(o, int64(0), "status", "desiredNumberScheduled")
	// Zero scheduled means "no node matched" — that is NOT proof of a
	// running workload, so it must never read as ready without the opt-in.
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("daemonset with 0 desired must not be ready (absence != success)")
	}
	if st := evalReadiness(o, true); !st.ready {
		t.Fatalf("daemonset with 0 desired and explicit opt-in should be ready: %q", st.reason)
	}
}

func TestReadinessDaemonSet(t *testing.T) {
	o := newObj("apps/v1", "DaemonSet", "demo", "ds")
	setNested(o, int64(1), "metadata", "generation")
	setNested(o, int64(1), "status", "observedGeneration")
	setNested(o, int64(3), "status", "desiredNumberScheduled")
	setNested(o, int64(2), "status", "numberReady")
	setNested(o, int64(3), "status", "updatedNumberScheduled")
	if st := evalReadiness(o, false); st.ready {
		t.Fatalf("2/3 ready must not be ready: %q", st.reason)
	}
	setNested(o, int64(3), "status", "numberReady")
	if st := evalReadiness(o, false); !st.ready {
		t.Fatalf("3/3 ready must be ready: %q", st.reason)
	}
}

func TestReadinessPod(t *testing.T) {
	running := newObj("v1", "Pod", "demo", "p")
	setNested(running, "Running", "status", "phase")
	if st := evalReadiness(running, false); st.ready {
		t.Fatal("running-but-not-Ready pod must not be ready")
	}
	setNested(running, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	if st := evalReadiness(running, false); !st.ready {
		t.Fatalf("running+Ready pod must be ready: %q", st.reason)
	}

	succeeded := newObj("v1", "Pod", "demo", "j")
	setNested(succeeded, "Succeeded", "status", "phase")
	if st := evalReadiness(succeeded, false); !st.ready {
		t.Fatal("succeeded pod must be ready")
	}

	failed := newObj("v1", "Pod", "demo", "f")
	setNested(failed, "Failed", "status", "phase")
	if st := evalReadiness(failed, false); st.terminal == nil {
		t.Fatal("failed pod must be a terminal failure, not a retry")
	}
}

func TestReadinessJob(t *testing.T) {
	o := newObj("batch/v1", "Job", "demo", "job")
	setNested(o, int64(2), "spec", "completions")
	setNested(o, int64(1), "status", "succeeded")
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("1/2 completions must not be ready")
	}
	setNested(o, int64(2), "status", "succeeded")
	if st := evalReadiness(o, false); !st.ready {
		t.Fatal("2/2 completions must be ready")
	}
}

func TestReadinessCRDRequiresEstablished(t *testing.T) {
	o := newObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "widgets.example.io")
	// Accepted by the apiserver but not Established -> not ready.
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("CRD without Established condition must not be ready")
	}
	setNested(o, []any{map[string]any{"type": "Established", "status": "False"}}, "status", "conditions")
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("CRD with Established=False must not be ready")
	}
	setNested(o, []any{map[string]any{"type": "Established", "status": "True"}}, "status", "conditions")
	if st := evalReadiness(o, false); !st.ready {
		t.Fatalf("CRD with Established=True must be ready: %q", st.reason)
	}
}

func TestReadinessDaemonSetAbsentDesiredNumberScheduled(t *testing.T) {
	o := newObj("apps/v1", "DaemonSet", "demo", "ds")
	setNested(o, int64(1), "metadata", "generation")
	setNested(o, int64(1), "status", "observedGeneration")

	// status.desiredNumberScheduled is ABSENT
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("daemonset with absent desiredNumberScheduled must not be ready when allowZero=false")
	}
	if st := evalReadiness(o, true); st.ready {
		t.Fatal("daemonset with absent desiredNumberScheduled must not be ready even when allowZero=true")
	}

	// Explicit present zero desired
	setNested(o, int64(0), "status", "desiredNumberScheduled")
	if st := evalReadiness(o, false); st.ready {
		t.Fatal("daemonset with explicit desired=0 must not be ready when allowZero=false")
	}
	if st := evalReadiness(o, true); !st.ready {
		t.Fatalf("daemonset with explicit desired=0 and allowZero=true should be ready: %q", st.reason)
	}
}

func TestReadinessJobFailedConditionIsTerminal(t *testing.T) {
	o := newObj("batch/v1", "Job", "demo", "job")
	setNested(o, int64(1), "spec", "completions")
	setNested(o, int64(0), "status", "succeeded")
	setNested(o, []any{map[string]any{"type": "Failed", "status": "True"}}, "status", "conditions")

	st := evalReadiness(o, false)
	if st.ready {
		t.Fatal("job with Failed=True condition must not be ready")
	}
	if st.terminal == nil {
		t.Fatal("job with Failed=True condition must be a terminal failure, not pending")
	}
}
