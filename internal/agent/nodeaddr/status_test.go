package nodeaddr

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/kubernetes/fake"
)

func textLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), &buf
}

// A reconciler that never started and one that started and fails every pass
// used to be INDISTINGUISHABLE in the log: the start was unannounced and every
// failure was logged at Debug, below the daemon's default level. That ambiguity
// is what made a control plane with no ExternalIP on any node hard to diagnose.
// Run must announce itself at Info.
func TestRun_AnnouncesStartAtInfo(t *testing.T) {
	ResetStatus()
	t.Cleanup(ResetStatus)

	log, buf := textLogger()
	cs := fake.NewSimpleClientset()
	r := &Reconciler{Client: cs, PortFor: DerivedKubeletPort, Log: log, Interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	// Run records Started immediately after emitting the Info line. Wait on the
	// synchronized status snapshot, then stop the writer before inspecting its
	// bytes.Buffer; reading the buffer while slog writes it is itself a race and
	// would make this observability test fail under `go test -race`.
	waitUntil(t, func() bool {
		s, _ := LastStatus()
		return s.Started
	})
	cancel()
	<-done
	if out := buf.String(); !strings.Contains(out, "reconciler started") {
		t.Errorf("reconciler start must be visible at Info; got:\n%s", out)
	}

	if s, ok := LastStatus(); !ok || !s.Started {
		t.Errorf("LastStatus() = %+v, ok=%v; want Started", s, ok)
	}
}

// A reconciler that is RUNNING but failing every pass must be visible at the
// default level too — it is a different problem from one that never started,
// with a different fix, and status must tell them apart.
func TestRun_WarnsOnFailureAndRecordsIt(t *testing.T) {
	ResetStatus()
	t.Cleanup(ResetStatus)

	log, buf := textLogger()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("apiserver unreachable")
	})
	r := &Reconciler{Client: cs, PortFor: DerivedKubeletPort, Log: log, Interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	waitUntil(t, func() bool {
		s, _ := LastStatus()
		return s.LastError != ""
	})
	cancel()
	<-done

	out := buf.String()
	if !strings.Contains(out, "reconcile failed") {
		t.Errorf("a failing pass must be visible at the default level; got:\n%s", out)
	}
	s, _ := LastStatus()
	if !s.Started {
		t.Errorf("a running-but-failing reconciler must still report Started")
	}
	if !strings.Contains(s.LastError, "apiserver unreachable") {
		t.Errorf("LastError = %q, want the underlying cause", s.LastError)
	}
	if s.ConsecutiveFailures < 1 {
		t.Errorf("ConsecutiveFailures = %d, want >= 1", s.ConsecutiveFailures)
	}
}

// A successful pass must clear the failure record, so a stale error cannot make
// a healthy reconciler look broken in `outpost status`.
func TestRun_ClearsErrorOnSuccess(t *testing.T) {
	ResetStatus()
	t.Cleanup(ResetStatus)

	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	r := &Reconciler{Client: cs, PortFor: DerivedKubeletPort, Interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	waitUntil(t, func() bool {
		s, _ := LastStatus()
		return !s.LastRunAt.IsZero()
	})
	cancel()
	<-done

	s, _ := LastStatus()
	if s.LastError != "" {
		t.Errorf("LastError = %q, want empty after a successful pass", s.LastError)
	}
	if s.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", s.ConsecutiveFailures)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
