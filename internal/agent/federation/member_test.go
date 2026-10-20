package federation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func memberNode(name string, addrs ...string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, a := range addrs {
		n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: a})
	}
	return n
}

func mirrorEndpoints(t *testing.T, cs *fake.Clientset) *corev1.Endpoints {
	t.Helper()
	ep, err := cs.CoreV1().Endpoints(KarmadaNamespace).Get(context.Background(), APIServerServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

// Kubernetes rejects loopback Endpoints (ValidateNonSpecialIP): a 127.0.0.1
// address — however it got there — is replaced by the node's InternalIP.
func TestEnsureMirrorReplacesLoopbackEndpoints(t *testing.T) {
	ctx := context.Background()
	stale := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: APIServerServiceName, Namespace: KarmadaNamespace},
		Subsets:    mirrorSubsets("127.0.0.1", 15443),
	}
	cs := fake.NewClientset(memberNode("member-b", "127.0.0.1", "10.88.0.7"), stale)

	ip, err := EnsureMirror(ctx, cs, "member-b", 15443)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.88.0.7" {
		t.Fatalf("mirror ip = %q, want the node InternalIP 10.88.0.7", ip)
	}
	ep := mirrorEndpoints(t, cs)
	if !subsetsMatch(ep.Subsets, "10.88.0.7", 15443) {
		t.Fatalf("endpoints not replaced: %+v", ep.Subsets)
	}
	svc, err := cs.CoreV1().Services(KarmadaNamespace).Get(ctx, APIServerServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Selector) != 0 {
		t.Fatalf("mirror service must be selectorless: %+v", svc.Spec.Selector)
	}
	if p := svc.Spec.Ports[0]; p.Port != 5443 || p.TargetPort.IntValue() != 15443 || p.Name != ep.Subsets[0].Ports[0].Name {
		t.Fatalf("service port = %+v, want 5443 -> 15443 named like the endpoints port", p)
	}
}

func TestEnsureMirrorRefusesLoopbackOnlyNode(t *testing.T) {
	cs := fake.NewClientset(memberNode("member-b", "127.0.0.1", "::1"))
	if _, err := EnsureMirror(context.Background(), cs, "member-b", 15443); err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("err = %v, want a no-usable-InternalIP refusal", err)
	}
	if _, err := cs.CoreV1().Endpoints(KarmadaNamespace).Get(context.Background(), APIServerServiceName, metav1.GetOptions{}); err == nil {
		t.Fatal("endpoints were created for a loopback-only node")
	}
}

// A recreated runtime container comes back on a new address: the next pass
// follows it.
func TestEnsureMirrorTracksChangedInternalIP(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset(memberNode("member-b", "10.88.0.7"))
	if _, err := EnsureMirror(ctx, cs, "member-b", 15443); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "member-b", metav1.GetOptions{})
	n.Status.Addresses[0].Address = "10.88.0.9"
	if _, err := cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMirror(ctx, cs, "member-b", 15443); err != nil {
		t.Fatal(err)
	}
	if ep := mirrorEndpoints(t, cs); !subsetsMatch(ep.Subsets, "10.88.0.9", 15443) {
		t.Fatalf("endpoints did not follow the node: %+v", ep.Subsets)
	}
}

// The plane hosting Karmada has the REAL operator Service (with a selector):
// never mirror over it.
func TestEnsureMirrorLeavesOperatorServiceAlone(t *testing.T) {
	real := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: APIServerServiceName, Namespace: KarmadaNamespace},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "karmada-apiserver"}},
	}
	cs := fake.NewClientset(memberNode("a", "10.88.0.7"), real)
	if _, err := EnsureMirror(context.Background(), cs, "a", 15443); err == nil {
		t.Fatal("mirror overwrote the operator's own Service")
	}
}

func TestRemoveMirrorOnlyDeletesOwnObjects(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset(memberNode("member-b", "10.88.0.7"))
	if _, err := EnsureMirror(ctx, cs, "member-b", 15443); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMirror(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Services(KarmadaNamespace).Get(ctx, APIServerServiceName, metav1.GetOptions{}); err == nil {
		t.Fatal("mirror service survived RemoveMirror")
	}
	if err := RemoveMirror(ctx, cs); err != nil {
		t.Fatalf("second RemoveMirror: %v", err)
	}
}

type recordRunner struct {
	calls [][]string
	err   error
}

func (r *recordRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	return nil, r.err
}

func agentKubeconfigSecret(t *testing.T, server string) *corev1.Secret {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["karmada"] = &clientcmdapi.Cluster{Server: server}
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: memberKubeconfigSecret, Namespace: KarmadaNamespace},
		Data:       map[string][]byte{memberKubeconfigSecretKV: b},
	}
}

func TestMemberReconcilerCompletesPendingJoin(t *testing.T) {
	ctx := context.Background()
	pending := filepath.Join(t.TempDir(), "outpost", "federation-join.json")
	if err := WritePendingJoin(pending, PendingJoin{BootstrapToken: "abcdef.0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(pending); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pending join file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	agent := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: AgentDeploymentName, Namespace: KarmadaNamespace}}
	agent.Status.ReadyReplicas = 1
	cs := fake.NewClientset(memberNode("member-b", "10.88.0.7"), agentKubeconfigSecret(t, "https://127.0.0.1:15443"), agent)
	runner := &recordRunner{}
	hash := strings.Repeat("ab", 32)
	visitorUp := false
	r := &MemberReconciler{
		Client: cs, NodeName: "member-b", ClusterName: "member-b", PlaneKubeconfig: "/plane.yaml",
		CAHash: "sha256:" + hash, Port: 15443, PendingPath: pending, Runner: runner,
		Dial: func(context.Context, int) error {
			if visitorUp {
				return nil
			}
			return errors.New("refused")
		},
	}

	// Visitor not bound yet: no register, the token stays pending.
	if err := r.Reconcile(ctx); err == nil || !strings.Contains(err.Error(), "waiting for the federation visitor") {
		t.Fatalf("err = %v, want waiting-for-visitor", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("registered before the visitor was up: %v", runner.calls)
	}
	if st := r.State(); !st.JoinPending || st.VisitorBound || st.MirrorIP != "10.88.0.7" {
		t.Fatalf("state = %+v", st)
	}

	visitorUp = true
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"register", "127.0.0.1:15443", "--token", "abcdef.0123456789abcdef",
		"--discovery-token-ca-cert-hash", "sha256:" + hash, "--cluster-name", "member-b", "--kubeconfig", "/plane.yaml"}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != strings.Join(want, " ") {
		t.Fatalf("karmadactl calls = %v, want %v", runner.calls, want)
	}
	if _, ok, _ := ReadPendingJoin(pending); ok {
		t.Fatal("bootstrap token not deleted after a successful register")
	}
	// Exception 1: the baked loopback endpoint is repointed at the Service.
	sec, _ := cs.CoreV1().Secrets(KarmadaNamespace).Get(ctx, memberKubeconfigSecret, metav1.GetOptions{})
	cfg, err := clientcmd.Load(sec.Data[memberKubeconfigSecretKV])
	if err != nil || cfg.Clusters["karmada"].Server != InClusterServer {
		t.Fatalf("agent kubeconfig server not rewritten: %v %+v", err, cfg)
	}
	// Exception 2: karmada-agent pinned to the visitor's node.
	d, _ := cs.AppsV1().Deployments(KarmadaNamespace).Get(ctx, AgentDeploymentName, metav1.GetOptions{})
	if d.Spec.Template.Spec.NodeSelector[hostnameLabel] != "member-b" {
		t.Fatalf("karmada-agent not pinned: %+v", d.Spec.Template.Spec.NodeSelector)
	}
	if st := r.State(); st.JoinPending || !st.VisitorBound || !st.SelfRegistered || !st.AgentReady || st.LastError != "" {
		t.Fatalf("state = %+v", st)
	}

	// Steady state: no second register.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("re-registered in steady state: %v", runner.calls)
	}
}

func TestMemberReconcilerFindsSuffixedAgentNode(t *testing.T) {
	ctx := context.Background()
	actual := "member-b-d99afcba"
	agent := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: AgentDeploymentName, Namespace: KarmadaNamespace}}
	cs := fake.NewClientset(memberNode(actual, "10.88.0.7"), agentKubeconfigSecret(t, "https://127.0.0.1:15443"), agent)
	r := &MemberReconciler{Client: cs, NodeName: "member-b", Port: 15443,
		PendingPath: filepath.Join(t.TempDir(), "pending.json"),
		Dial:        func(context.Context, int) error { return errors.New("not ready") },
	}
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.State().MirrorIP; got != "10.88.0.7" {
		t.Fatalf("mirror IP = %q, want agent node IP", got)
	}
	if ep := mirrorEndpoints(t, cs); !subsetsMatch(ep.Subsets, "10.88.0.7", 15443) {
		t.Fatalf("mirror endpoint = %+v, want agent node", ep.Subsets)
	}
	d, err := cs.AppsV1().Deployments(KarmadaNamespace).Get(ctx, AgentDeploymentName, metav1.GetOptions{})
	if err != nil || d.Spec.Template.Spec.NodeSelector[hostnameLabel] != actual {
		t.Fatalf("agent node selector = %+v, err = %v", d.Spec.Template.Spec.NodeSelector, err)
	}
}

func TestWritePendingJoinRejectsMalformedToken(t *testing.T) {
	if err := WritePendingJoin(filepath.Join(t.TempDir(), "j.json"), PendingJoin{BootstrapToken: "not-a-token"}); err == nil {
		t.Fatal("malformed bootstrap token accepted")
	}
}

func TestLeaveMemberUnregistersThenRemovesMirror(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("federation verbs refuse on windows")
	}
	ctx := context.Background()
	cs := fake.NewClientset(memberNode("member-b", "10.88.0.7"))
	if _, err := EnsureMirror(ctx, cs, "member-b", 15443); err != nil {
		t.Fatal(err)
	}
	runner := &recordRunner{}
	if err := LeaveMember(ctx, runner, cs, "member-b", "/plane.yaml", "/host-karmada.yaml"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls[0], " "); got != "unregister member-b --cluster-kubeconfig /plane.yaml --karmada-config /host-karmada.yaml" {
		t.Fatalf("unregister call = %q", got)
	}
	if _, err := cs.CoreV1().Services(KarmadaNamespace).Get(ctx, APIServerServiceName, metav1.GetOptions{}); err == nil {
		t.Fatal("mirror service survived leave")
	}

	// A failed unregister keeps the mirror: a retry needs it to reach Karmada.
	cs2 := fake.NewClientset(memberNode("member-b", "10.88.0.7"))
	_, _ = EnsureMirror(ctx, cs2, "member-b", 15443)
	if err := LeaveMember(ctx, &recordRunner{err: errors.New("boom")}, cs2, "member-b", "/plane.yaml", "/host-karmada.yaml"); err == nil {
		t.Fatal("leave swallowed an unregister failure")
	}
	if _, err := cs2.CoreV1().Services(KarmadaNamespace).Get(ctx, APIServerServiceName, metav1.GetOptions{}); err != nil {
		t.Fatal("mirror removed although unregister failed")
	}
}

func TestCheckOSForRefusesWindows(t *testing.T) {
	if err := CheckOSFor("windows"); !errors.Is(err, ErrUnsupportedOS) {
		t.Fatalf("windows: err = %v, want ErrUnsupportedOS", err)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if err := CheckOSFor(goos); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
	}
}
