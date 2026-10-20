package federation

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// AgentDeploymentName is the Deployment `karmadactl register` creates on a
// member plane (its --agent-name default).
const AgentDeploymentName = "karmada-agent"

// hostnameLabel is the well-known node label exception 2 pins on.
const hostnameLabel = "kubernetes.io/hostname"

// CheckOSFor is CheckOS for an explicit GOOS, so the Windows refusal is
// testable on every platform.
func CheckOSFor(goos string) error {
	if goos == "windows" {
		return ErrUnsupportedOS
	}
	return nil
}

// RegisterMember registers a member plane in pull mode: `karmadactl register`
// dials endpoint (always the loopback visitor, 127.0.0.1:<port>) for discovery
// and installs karmada-agent into the plane reached by planeKubeconfig. The
// endpoint is also baked into the agent's Secret — §7 exception 1 rewrites it
// afterwards (RewriteMemberAgentKubeconfig).
func RegisterMember(ctx context.Context, r Runner, endpoint, bootstrapToken, caHash, clusterName, planeKubeconfig string) error {
	hash := NormalizeHash(caHash)
	if hash == "" {
		return fmt.Errorf("invalid CA hash %q", caHash)
	}
	_, err := r.Run(ctx, "register", endpoint,
		"--token", bootstrapToken,
		"--discovery-token-ca-cert-hash", "sha256:"+hash,
		"--cluster-name", clusterName,
		"--kubeconfig", planeKubeconfig)
	return err
}

// UnregisterMember runs `karmadactl unregister`. The member admin kubeconfig is
// required; karmadaConfig (the host's materialised admin kubeconfig) is passed
// when known so the control-plane RBAC is cleaned up too (§9.5). A member has
// no host kubeconfig, so it leaves it empty and karmadactl reads the
// credential off the agent Deployment — which dials the mirror Service, so
// leave must run while the visitor is still up.
func UnregisterMember(ctx context.Context, r Runner, clusterName, planeKubeconfig, karmadaConfig string) error {
	args := []string{"unregister", clusterName, "--cluster-kubeconfig", planeKubeconfig}
	if karmadaConfig != "" {
		args = append(args, "--karmada-config", karmadaConfig)
	}
	_, err := r.Run(ctx, args...)
	return err
}

// PinAgentToNode applies §7 exception 2: a nodeSelector on Deployment
// karmada-agent so the pod lands on the node whose runtime container holds the
// federation visitor (the mirror Endpoints point at that node). A no-op when
// the selector is already in place; NotFound is returned as-is so callers can
// tell "not registered yet".
func PinAgentToNode(ctx context.Context, member kubernetes.Interface, nodeName string) error {
	deps := member.AppsV1().Deployments(KarmadaNamespace)
	d, err := deps.Get(ctx, AgentDeploymentName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if d.Spec.Template.Spec.NodeSelector[hostnameLabel] == nodeName {
		return nil
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"nodeSelector": map[string]string{hostnameLabel: nodeName},
		}}},
	})
	_, err = deps.Patch(ctx, AgentDeploymentName, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// AgentReady reports whether the member's karmada-agent Deployment has a ready
// replica. A missing Deployment is not ready, not an error.
func AgentReady(ctx context.Context, member kubernetes.Interface) (exists, ready bool, err error) {
	d, err := member.AppsV1().Deployments(KarmadaNamespace).Get(ctx, AgentDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, d.Status.ReadyReplicas > 0, nil
}
