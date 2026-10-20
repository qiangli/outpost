package federation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func testCA(t *testing.T) (caPEM []byte, wantHash string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "karmada"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(sum[:])
}

func adminKubeconfig(t *testing.T, ca []byte, server string) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["karmada-apiserver"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: ca}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["c"] = &clientcmdapi.Context{Cluster: "karmada-apiserver", AuthInfo: "admin"}
	cfg.CurrentContext = "c"
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCAHashIsSPKIDiscoveryHash(t *testing.T) {
	ca, want := testCA(t)
	got, err := CAHashFromKubeconfig(adminKubeconfig(t, ca, "https://10.0.0.1:5443"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	if id := FederationID("sha256:" + strings.ToUpper(got)); id != "fed-"+want[:12] {
		t.Fatalf("id = %q", id)
	}
	if FederationID("nothex") != "" || NormalizeHash("abc") != "" {
		t.Fatal("invalid hashes must normalise to empty")
	}
	if _, err := CAHashFromKubeconfig([]byte("apiVersion: v1\nkind: Config\n")); err == nil {
		t.Fatal("kubeconfig without a CA must error")
	}
}

func TestMaterializeRewritesServerWith0600AndIsStable(t *testing.T) {
	ca, _ := testCA(t)
	admin := adminKubeconfig(t, ca, "https://karmada-apiserver.karmada-system.svc.cluster.local:5443")
	path := filepath.Join(t.TempDir(), "outpost-federation", "karmada.yaml")

	out, changed, err := Materialize(path, admin, 15443)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "https://127.0.0.1:15443") || strings.Contains(string(out), ".svc") {
		t.Fatalf("server not rewritten:\n%s", out)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if d, _ := os.Stat(filepath.Dir(path)); d.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", d.Mode().Perm())
	}
	if _, changed, err = Materialize(path, admin, 15443); err != nil || changed {
		t.Fatalf("idempotent rewrite: changed=%v err=%v", changed, err)
	}
	if _, changed, _ = Materialize(path, admin, 25443); !changed {
		t.Fatal("a port change must rewrite")
	}
	if err := Remove(path); err != nil || Remove(path) != nil {
		t.Fatal("Remove must be idempotent")
	}
}

func TestParseRegisterCommand(t *testing.T) {
	h := strings.Repeat("ab", 32)
	rc, err := ParseRegisterCommand("karmadactl register 127.0.0.1:15443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:" + strings.ToUpper(h) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if rc.Endpoint != "127.0.0.1:15443" || rc.Token != "abcdef.0123456789abcdef" || rc.CAHash != h {
		t.Fatalf("%+v", rc)
	}
	if _, err := ParseRegisterCommand("nothing"); err == nil {
		t.Fatal("garbage must error")
	}
	if got := redactTokens("x --token abcdef.0123456789abcdef y"); strings.Contains(got, "abcdef") {
		t.Fatalf("token leaked: %s", got)
	}
}

type fakeRunner struct {
	calls [][]string
	hash  string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	if args[0] == "token" {
		return []byte("karmadactl register 127.0.0.1:15443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:" + f.hash), nil
	}
	return nil, nil
}

type fakePlane struct {
	phase  KarmadaPhase
	admin  []byte
	adminE error
}

func (p *fakePlane) KarmadaPhase(context.Context) (KarmadaPhase, error) { return p.phase, nil }
func (p *fakePlane) AdminConfig(context.Context) ([]byte, error)        { return p.admin, p.adminE }

func TestHostReconcilerConvergesInOrder(t *testing.T) {
	ca, hash := testCA(t)
	plane := &fakePlane{phase: PhaseAbsent, adminE: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, AdminSecretName)}
	runner := &fakeRunner{hash: hash}
	var installs, posts int
	var recorded []string
	var members []Member
	r := &HostReconciler{
		Plane: plane, Runner: runner, Port: 15443, ClusterName: "host-a",
		KubeconfigPath: filepath.Join(t.TempDir(), "k", "karmada.yaml"), PlaneKubeconfig: "/plane.yaml",
		Install:      func(context.Context) error { installs++; plane.phase = PhaseInstalling; return nil },
		Members:      func(context.Context, string) ([]Member, error) { return members, nil },
		RecordHash:   func(h string) error { recorded = append(recorded, h); return nil },
		PostRegister: func(context.Context) error { posts++; return nil },
	}
	ctx := context.Background()

	// Pass 1: CR absent -> install; admin secret not there yet is a delay.
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if installs != 1 || r.State().Phase != PhaseInstalling {
		t.Fatalf("installs=%d state=%+v", installs, r.State())
	}
	if _, err := os.Stat(r.KubeconfigPath); err == nil {
		t.Fatal("kubeconfig materialised before the operator produced it")
	}

	// Pass 2: secret appears but CR not Ready -> identity learned, NOT registered.
	plane.adminE, plane.admin = nil, adminKubeconfig(t, ca, "https://svc:5443")
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0] != hash || len(runner.calls) != 0 {
		t.Fatalf("recorded=%v calls=%v", recorded, runner.calls)
	}

	// Pass 3: Ready -> self-register once, with the in-cluster fixup.
	plane.phase = PhaseReady
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if installs != 1 || posts != 1 || len(runner.calls) != 2 {
		t.Fatalf("installs=%d posts=%d calls=%v", installs, posts, runner.calls)
	}
	reg := strings.Join(runner.calls[1], " ")
	for _, want := range []string{"register 127.0.0.1:15443", "--cluster-name host-a", "--kubeconfig /plane.yaml", "sha256:" + hash} {
		if !strings.Contains(reg, want) {
			t.Errorf("register argv missing %q: %s", want, reg)
		}
	}
	if len(recorded) != 1 {
		t.Fatalf("hash re-recorded: %v", recorded)
	}

	// Pass 4: already a member -> nothing more runs.
	members = []Member{{Name: "host-a", Ready: true}}
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || len(runner.calls) != 2 || !r.State().SelfRegistered {
		t.Fatalf("re-registered: posts=%d calls=%d", posts, len(runner.calls))
	}
	state := r.State()
	if len(state.Members) != 1 || state.Members[0].Name != "host-a" || !state.Members[0].Ready {
		t.Fatalf("member readiness missing from status: %+v", state.Members)
	}
}

func TestHostReconcilerReportsRegisterFailure(t *testing.T) {
	ca, hash := testCA(t)
	r := &HostReconciler{
		Plane:  &fakePlane{phase: PhaseReady, admin: adminKubeconfig(t, ca, "https://svc:5443")},
		Runner: &fakeRunner{hash: hash, err: errors.New("dial 127.0.0.1:15443: refused")},
		Port:   15443, ClusterName: "h", KubeconfigPath: filepath.Join(t.TempDir(), "k.yaml"),
		Members: func(context.Context, string) ([]Member, error) { return nil, nil },
	}
	err := r.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	if r.State().SelfRegistered || r.State().LastError == "" {
		t.Fatalf("failure must not read as registered: %+v", r.State())
	}
}

func TestRewriteMemberAgentKubeconfigRepointsAtService(t *testing.T) {
	ca, _ := testCA(t)
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: KarmadaNamespace, Name: "karmada-kubeconfig"},
		Data:       map[string][]byte{"karmada.config": adminKubeconfig(t, ca, "https://127.0.0.1:15443")},
	})
	if err := RewriteMemberAgentKubeconfig(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	sec, _ := cs.CoreV1().Secrets(KarmadaNamespace).Get(context.Background(), "karmada-kubeconfig", metav1.GetOptions{})
	if !strings.Contains(string(sec.Data["karmada.config"]), InClusterServer) {
		t.Fatalf("not rewritten:\n%s", sec.Data["karmada.config"])
	}
	if err := RewriteMemberAgentKubeconfig(context.Background(), cs); err != nil {
		t.Fatalf("second pass must be a no-op: %v", err)
	}
}
