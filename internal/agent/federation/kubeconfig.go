package federation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Where the Karmada operator stores the admin kubeconfig it generates for a
// Karmada CR named "karmada" (design 9.3, verified at v1.19.0).
const (
	KarmadaNamespace         = "karmada-system"
	AdminSecretName          = "karmada-admin-config"
	AdminSecretKey           = "karmada.config"
	APIServerServiceName     = "karmada-apiserver"
	APIServerServicePort     = 5443
	InClusterServer          = "https://karmada-apiserver.karmada-system.svc:5443"
	memberKubeconfigSecret   = "karmada-kubeconfig"
	memberKubeconfigSecretKV = "karmada.config"
)

// LoopbackServer is the apiserver URL the materialised kubeconfig dials. Only
// 127.0.0.1 and karmada-apiserver.karmada-system.svc are ever in the cert SANs.
func LoopbackServer(port int) string { return fmt.Sprintf("https://127.0.0.1:%d", port) }

// RewriteServer points every cluster in the kubeconfig at server.
func RewriteServer(kubeconfig []byte, server string) ([]byte, error) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("kubeconfig has no clusters")
	}
	for _, c := range cfg.Clusters {
		c.Server = server
	}
	return clientcmd.Write(*cfg)
}

// FetchAdminConfig reads the operator-generated admin kubeconfig from the
// hosting plane. The error wraps the API error so callers can tell "not yet"
// (NotFound) from a real failure.
func FetchAdminConfig(ctx context.Context, cs kubernetes.Interface) ([]byte, error) {
	sec, err := cs.CoreV1().Secrets(KarmadaNamespace).Get(ctx, AdminSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	b := sec.Data[AdminSecretKey]
	if len(b) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no %q key", KarmadaNamespace, AdminSecretName, AdminSecretKey)
	}
	return b, nil
}

// Materialize rewrites the admin kubeconfig to the loopback federation port and
// writes it 0600 (dir 0700) atomically at path. changed reports whether the
// file content differs from before, so callers do not churn mtimes.
func Materialize(path string, adminConfig []byte, port int) (rewritten []byte, changed bool, err error) {
	rewritten, err = RewriteServer(adminConfig, LoopbackServer(port))
	if err != nil {
		return nil, false, err
	}
	if old, rerr := os.ReadFile(path); rerr == nil && string(old) == string(rewritten) {
		if info, serr := os.Stat(path); serr == nil && info.Mode().Perm() == 0o600 {
			return rewritten, false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".karmada-*.yaml")
	if err != nil {
		return nil, false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, false, err
	}
	if _, err := tmp.Write(rewritten); err != nil {
		tmp.Close()
		return nil, false, err
	}
	if err := tmp.Close(); err != nil {
		return nil, false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, false, err
	}
	return rewritten, true, nil
}

// Remove deletes a materialised kubeconfig; a missing file is not an error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RewriteMemberAgentKubeconfig applies design exception 1 on a member plane
// (including the host's own plane): the Secret karmadactl register bakes for
// the agent names 127.0.0.1:<port>, which is meaningless inside a pod, so it
// is repointed at the Service name the Karmada cert carries as a SAN.
func RewriteMemberAgentKubeconfig(ctx context.Context, member kubernetes.Interface) error {
	secrets := member.CoreV1().Secrets(KarmadaNamespace)
	sec, err := secrets.Get(ctx, memberKubeconfigSecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cur := sec.Data[memberKubeconfigSecretKV]
	if len(cur) == 0 {
		return fmt.Errorf("secret %s/%s has no %q key", KarmadaNamespace, memberKubeconfigSecret, memberKubeconfigSecretKV)
	}
	next, err := RewriteServer(cur, InClusterServer)
	if err != nil {
		return err
	}
	if string(next) == string(cur) {
		return nil
	}
	sec.Data[memberKubeconfigSecretKV] = next
	_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
	return err
}
