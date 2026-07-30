package vknode

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	// NativeArtifactCredentialProfileAnnotation references a runtime-only
	// Secret in the Pod's own namespace. The portable workflow carries only
	// this opaque profile name; credentials never enter Pod annotations.
	NativeArtifactCredentialProfileAnnotation = "outpost.dhnt.io/native-artifact-credential-profile"

	NativeArtifactCredentialSecretType corev1.SecretType = "outpost.dhnt.io/native-artifact-profile"

	nativeArtifactProfileKindKey      = "kind"
	nativeArtifactProfileScopeKey     = "scope"
	nativeArtifactProfileAccessKeyKey = "access-key"
	nativeArtifactProfileSecretKeyKey = "secret-key"
	nativeArtifactProfileRegionKey    = "region"

	nativeArtifactProfileKindAWSSigV4 = "aws-sigv4"
)

// NativeArtifactCredentialProfile is the resolved runtime credential. It is
// deliberately absent from Pod status, evidence, cache keys, and errors.
type NativeArtifactCredentialProfile struct {
	Kind      string
	Scope     *url.URL
	AccessKey string
	SecretKey string
	Region    string
}

// NativeArtifactCredentialResolver binds an opaque profile name at the
// execution venue. Implementations must scope the lookup to podNamespace.
type NativeArtifactCredentialResolver interface {
	ResolveNativeArtifactCredential(
		ctx context.Context,
		podNamespace string,
		profileName string,
	) (NativeArtifactCredentialProfile, error)
}

type kubernetesNativeArtifactCredentialResolver struct {
	client kubernetes.Interface
}

func (r *kubernetesNativeArtifactCredentialResolver) ResolveNativeArtifactCredential(
	ctx context.Context,
	podNamespace string,
	profileName string,
) (NativeArtifactCredentialProfile, error) {
	var zero NativeArtifactCredentialProfile
	if r == nil || r.client == nil {
		return zero, errors.New("vknode: native artifact credential resolver unavailable")
	}
	podNamespace = strings.TrimSpace(podNamespace)
	profileName = strings.TrimSpace(profileName)
	if podNamespace == "" || profileName == "" ||
		len(validation.IsDNS1123Subdomain(profileName)) != 0 {
		return zero, errors.New("vknode: invalid native artifact credential profile reference")
	}
	secret, err := r.client.CoreV1().Secrets(podNamespace).Get(ctx, profileName, metav1.GetOptions{})
	if err != nil {
		// Do not reflect Kubernetes response bodies: they may carry backend or
		// admission details irrelevant to the portable execution evidence.
		return zero, fmt.Errorf("vknode: native artifact credential profile %q unavailable", profileName)
	}
	if secret.Type != NativeArtifactCredentialSecretType {
		return zero, fmt.Errorf("vknode: native artifact credential profile %q has wrong Secret type", profileName)
	}
	profile, err := nativeArtifactProfileFromSecret(secret)
	if err != nil {
		return zero, fmt.Errorf("vknode: native artifact credential profile %q invalid: %w", profileName, err)
	}
	return profile, nil
}

func nativeArtifactProfileFromSecret(secret *corev1.Secret) (NativeArtifactCredentialProfile, error) {
	var profile NativeArtifactCredentialProfile
	if secret == nil {
		return profile, errors.New("nil Secret")
	}
	profile.Kind = strings.TrimSpace(string(secret.Data[nativeArtifactProfileKindKey]))
	if profile.Kind != nativeArtifactProfileKindAWSSigV4 {
		return profile, errors.New("unsupported kind")
	}
	rawScope := strings.TrimSpace(string(secret.Data[nativeArtifactProfileScopeKey]))
	scope, err := parseNativeArtifactScope(rawScope)
	if err != nil {
		return profile, err
	}
	profile.Scope = scope
	profile.AccessKey = strings.TrimSpace(string(secret.Data[nativeArtifactProfileAccessKeyKey]))
	profile.SecretKey = strings.TrimSpace(string(secret.Data[nativeArtifactProfileSecretKeyKey]))
	profile.Region = strings.TrimSpace(string(secret.Data[nativeArtifactProfileRegionKey]))
	if profile.AccessKey == "" || profile.SecretKey == "" {
		return profile, errors.New("access-key and secret-key are required")
	}
	if profile.Region == "" {
		profile.Region = "us-east-1"
	}
	return profile, nil
}

func parseNativeArtifactScope(raw string) (*url.URL, error) {
	scope, err := url.Parse(raw)
	if err != nil || scope.Scheme == "" || scope.Host == "" {
		return nil, errors.New("scope must be an absolute URL")
	}
	if scope.Scheme != "https" && !(scope.Scheme == "http" && isLoopbackHost(scope.Hostname())) {
		return nil, errors.New("scope must use https")
	}
	if scope.User != nil || scope.RawQuery != "" || scope.Fragment != "" {
		return nil, errors.New("scope must not contain userinfo, query, or fragment")
	}
	scope.Path = cleanURLPath(scope.Path)
	return scope, nil
}
