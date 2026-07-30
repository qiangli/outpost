package vknode

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubernetesArtifactProfileIsNamespaceScoped(t *testing.T) {
	client := fake.NewSimpleClientset(
		artifactProfileSecret("user-a", "nanochat-read", "https://artifacts.example.test/user-a/"),
		artifactProfileSecret("user-b", "nanochat-read", "https://artifacts.example.test/user-b/"),
	)
	resolver := &kubernetesNativeArtifactCredentialResolver{client: client}
	got, err := resolver.ResolveNativeArtifactCredential(
		context.Background(), "user-a", "nanochat-read",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope.String() != "https://artifacts.example.test/user-a/" {
		t.Fatalf("resolved cross-namespace profile scope %q", got.Scope)
	}
}

func TestKubernetesArtifactProfileRejectsMissingAndWrongType(t *testing.T) {
	wrongType := artifactProfileSecret(
		"user-a", "wrong-type", "https://artifacts.example.test/user-a/",
	)
	wrongType.Type = corev1.SecretTypeOpaque
	resolver := &kubernetesNativeArtifactCredentialResolver{
		client: fake.NewSimpleClientset(wrongType),
	}
	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{name: "missing", ref: "missing", want: "unavailable"},
		{name: "wrong type", ref: "wrong-type", want: "wrong Secret type"},
		{name: "cross namespace syntax", ref: "user-b/profile", want: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolver.ResolveNativeArtifactCredential(
				context.Background(), "user-a", tc.ref,
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestArtifactProfileRequiresBoundedQueryFreeScope(t *testing.T) {
	for _, rawScope := range []string{
		"https://artifacts.example.test/path/?token=secret",
		"https://user:pass@artifacts.example.test/path/",
		"http://artifacts.example.test/path/",
	} {
		secret := artifactProfileSecret("user-a", "bad", rawScope)
		_, err := nativeArtifactProfileFromSecret(secret)
		if err == nil {
			t.Fatalf("accepted unsafe scope %q", rawScope)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "pass") {
			t.Fatalf("scope validation leaked credential material: %v", err)
		}
	}
}

func TestConfiguredArtifactBrokerIsNotReplacedByDefaultResolver(t *testing.T) {
	configured := &staticNativeArtifactCredentialResolver{}
	fallback := &staticNativeArtifactCredentialResolver{}
	raw, err := NewNativeProcessBackend(NativeProcessConfig{
		DataDir:             t.TempDir(),
		ArtifactCredentials: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := raw.(*nativeProcessBackend)
	backend.setNativeArtifactCredentialResolver(fallback)
	if backend.artifactCredentials != configured {
		t.Fatal("venue-configured credential broker was replaced by Kubernetes default")
	}
}

func artifactProfileSecret(namespace, name, scope string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Type:       NativeArtifactCredentialSecretType,
		Data: map[string][]byte{
			nativeArtifactProfileKindKey:      []byte(nativeArtifactProfileKindAWSSigV4),
			nativeArtifactProfileScopeKey:     []byte(scope),
			nativeArtifactProfileAccessKeyKey: []byte("access"),
			nativeArtifactProfileSecretKeyKey: []byte("secret"),
			nativeArtifactProfileRegionKey:    []byte("us-east-1"),
		},
	}
}
