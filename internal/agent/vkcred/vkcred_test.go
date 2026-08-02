package vkcred

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func validBundle() Bundle {
	return Bundle{
		CA:         []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
		Token:      "sa-bearer-token",
		Namespaces: []string{"default", "workloads"},
	}
}

func TestBundleRoundTrip(t *testing.T) {
	in := validBundle()
	enc, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(enc, bundlePrefix) {
		t.Errorf("encoded bundle missing version prefix: %q", enc)
	}
	// One line, no spaces — the operator pastes this into a shell.
	if strings.ContainsAny(enc, " \n\t") {
		t.Errorf("encoded bundle is not paste-safe: %q", enc)
	}
	out, err := Decode("  " + enc + "\n") // surrounding space survives a paste
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestBundleDecodeRejects(t *testing.T) {
	good, _ := validBundle().Encode()
	for name, s := range map[string]string{
		"empty":            "",
		"no prefix":        "eyJ0b2tlbiI6IngifQ",
		"wrong value":      "K10abcdef::node:secret", // the k3s node token, the likeliest mispaste
		"truncated":        good[:len(good)-7],
		"garbage after ok": bundlePrefix + "!!!not-base64url!!!",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(s); err == nil {
				t.Errorf("Decode(%q) accepted", s)
			}
		})
	}
}

func TestBundleValidateFailClosed(t *testing.T) {
	for name, mut := range map[string]func(*Bundle){
		"no CA":            func(b *Bundle) { b.CA = nil },
		"no token":         func(b *Bundle) { b.Token = " " },
		"no namespaces":    func(b *Bundle) { b.Namespaces = nil },
		"empty namespaces": func(b *Bundle) { b.Namespaces = []string{} },
		"bad namespace":    func(b *Bundle) { b.Namespaces = []string{"Not_A_Label"} },
	} {
		t.Run(name, func(t *testing.T) {
			b := validBundle()
			mut(&b)
			if err := b.Validate(); err == nil {
				t.Error("Validate accepted an unusable bundle")
			}
			if _, err := b.Encode(); err == nil {
				t.Error("Encode produced an unusable bundle")
			}
		})
	}
}

// populateTokenSecret installs the fake-clientset stand-in for the plane's
// token controller: every Get of the token secret returns it populated.
func populateTokenSecret(cs *fake.Clientset, token string, ca []byte) {
	cs.PrependReactor("get", "secrets", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		get, ok := action.(ktesting.GetAction)
		if !ok || get.GetName() != TokenSecretName || get.GetNamespace() != SystemNamespace {
			return false, nil, nil
		}
		return true, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: TokenSecretName, Namespace: SystemNamespace},
			Type:       corev1.SecretTypeServiceAccountToken,
			Data: map[string][]byte{
				corev1.ServiceAccountTokenKey:  []byte(token),
				corev1.ServiceAccountRootCAKey: ca,
			},
		}, nil
	})
}

func TestMintProvisionsLeastPrivilege(t *testing.T) {
	cs := fake.NewClientset()
	populateTokenSecret(cs, "minted-token", []byte("peer-ca"))

	b, err := Mint(context.Background(), MintOptions{
		Client:     cs,
		Namespaces: []string{"workloads", " workloads ", "default"}, // dup + spaces normalize away
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if b.Token != "minted-token" || string(b.CA) != "peer-ca" {
		t.Errorf("bundle credential = token %q / ca %q", b.Token, b.CA)
	}
	if !reflect.DeepEqual(b.Namespaces, []string{"workloads", "default"}) {
		t.Errorf("bundle namespaces = %v", b.Namespaces)
	}

	ctx := context.Background()
	for _, ns := range []string{SystemNamespace, "workloads", "default"} {
		if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
			t.Errorf("namespace %s not provisioned: %v", ns, err)
		}
	}
	if _, err := cs.CoreV1().ServiceAccounts(SystemNamespace).Get(ctx, ServiceAccountName, metav1.GetOptions{}); err != nil {
		t.Errorf("service account not provisioned: %v", err)
	}

	role, err := cs.RbacV1().ClusterRoles().Get(ctx, ClusterRoleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role not provisioned: %v", err)
	}
	// LEAST PRIVILEGE IS THE CONTRACT: the exact rule set, so a widened verb
	// shows up as a failing diff in review, not a silent capability grant.
	if !reflect.DeepEqual(role.Rules, vkClusterRoleRules) {
		t.Errorf("cluster role rules drifted:\n got %+v\nwant %+v", role.Rules, vkClusterRoleRules)
	}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res == "secrets" || res == "configmaps" {
				for _, v := range rule.Verbs {
					switch v {
					case "get", "list", "watch":
					default:
						t.Errorf("write verb %q on %s — the vk credential must stay read-only there", v, res)
					}
				}
			}
		}
	}

	crb, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, ClusterRoleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding not provisioned: %v", err)
	}
	if crb.RoleRef.Name != ClusterRoleName ||
		len(crb.Subjects) != 1 || crb.Subjects[0].Name != ServiceAccountName ||
		crb.Subjects[0].Namespace != SystemNamespace {
		t.Errorf("binding wires the wrong identity: %+v", crb)
	}
}

func TestMintIsIdempotentAndConvergesTheRole(t *testing.T) {
	cs := fake.NewClientset()
	populateTokenSecret(cs, "tok", []byte("ca"))
	ctx := context.Background()

	if _, err := Mint(ctx, MintOptions{Client: cs, Namespaces: []string{"default"}}); err != nil {
		t.Fatalf("first Mint: %v", err)
	}

	// Simulate drift: someone widened the role by hand.
	role, _ := cs.RbacV1().ClusterRoles().Get(ctx, ClusterRoleName, metav1.GetOptions{})
	role.Rules = append(role.Rules, vkClusterRoleRules[0])
	role.Rules[0].Verbs = append(role.Rules[0].Verbs, "deletecollection")
	if _, err := cs.RbacV1().ClusterRoles().Update(ctx, role, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	b, err := Mint(ctx, MintOptions{Client: cs, Namespaces: []string{"default"}})
	if err != nil {
		t.Fatalf("second Mint: %v", err)
	}
	if b.Token != "tok" {
		t.Errorf("re-mint changed the token: %q", b.Token)
	}
	role, _ = cs.RbacV1().ClusterRoles().Get(ctx, ClusterRoleName, metav1.GetOptions{})
	if !reflect.DeepEqual(role.Rules, vkClusterRoleRules) {
		t.Errorf("re-mint did not converge the drifted role: %+v", role.Rules)
	}
}

func TestMintRequiresNamespaces(t *testing.T) {
	cs := fake.NewClientset()
	populateTokenSecret(cs, "tok", []byte("ca"))
	for name, nss := range map[string][]string{
		"none":       nil,
		"only blank": {"", "  "},
		"invalid":    {"Bad_Name"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Mint(context.Background(), MintOptions{Client: cs, Namespaces: nss}); err == nil {
				t.Error("Mint accepted an unusable namespace policy")
			}
		})
	}
}

func TestMintTimesOutWhenTokenNeverPopulates(t *testing.T) {
	cs := fake.NewClientset() // no reactor: the secret stays empty, as on a plane with a broken token controller
	_, err := Mint(context.Background(), MintOptions{
		Client:       cs,
		Namespaces:   []string{"default"},
		PollInterval: time.Millisecond,
		Timeout:      20 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "token secret") {
		t.Errorf("want token-secret timeout, got %v", err)
	}
}
