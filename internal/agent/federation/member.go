package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

// Member side of the federation (design §5.4). `federation join` persists the
// host endpoint, tunnel token, STCP secret and CA hash in agent.json, but the
// bootstrap token — 24 h, single use — only in a 0600 pending-join file under
// the cache dir, deleted once `karmadactl register` succeeds. The daemon's
// member reconciler finishes the join after the restart that brings the
// visitor up.

// PendingJoin is the pending-join file's content.
type PendingJoin struct {
	BootstrapToken string    `json:"bootstrap_token"`
	CreatedAt      time.Time `json:"created_at"`
}

// PendingJoinRel is the pending-join file relative to the user cache dir.
const PendingJoinRel = "outpost/federation-join.json"

// DefaultPendingJoinPath resolves PendingJoinRel under os.UserCacheDir.
func DefaultPendingJoinPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(PendingJoinRel)), nil
}

// ValidBootstrapToken reports whether t has the kubeadm token shape
// [a-z0-9]{6}.[a-z0-9]{16}.
func ValidBootstrapToken(t string) bool {
	id, secret, ok := strings.Cut(t, ".")
	return ok && len(id) == 6 && len(secret) == 16 && lowerAlnum(id) && lowerAlnum(secret)
}

func lowerAlnum(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// WritePendingJoin writes p atomically with mode 0600 (dir 0700).
func WritePendingJoin(path string, p PendingJoin) error {
	if !ValidBootstrapToken(p.BootstrapToken) {
		return fmt.Errorf("bootstrap token must look like abcdef.0123456789abcdef")
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".federation-join-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadPendingJoin returns the pending join; ok is false when there is none.
func ReadPendingJoin(path string) (p PendingJoin, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return PendingJoin{}, false, nil
	}
	if err != nil {
		return PendingJoin{}, false, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return PendingJoin{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return p, true, nil
}

// RemovePendingJoin deletes the pending-join file; missing is fine.
func RemovePendingJoin(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// DialVisitor reports whether the loopback visitor accepts a connection.
func DialVisitor(ctx context.Context, port int) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, "tcp", LoopbackEndpoint(port))
	if err != nil {
		return err
	}
	return c.Close()
}

// LeaveMember undoes a join on the member plane: `karmadactl unregister`
// (removes karmada-agent and the Cluster object, through the still-running
// visitor), then the mirror Service/Endpoints. Unregister runs first because
// its auto-fetched control-plane credential dials the mirror.
func LeaveMember(ctx context.Context, r Runner, cs kubernetes.Interface, clusterName, planeKubeconfig, karmadaConfig string) error {
	if err := CheckOS(); err != nil {
		return err
	}
	if err := UnregisterMember(ctx, r, clusterName, planeKubeconfig, karmadaConfig); err != nil {
		return err
	}
	if err := RemoveMirror(ctx, cs); err != nil {
		return fmt.Errorf("remove mirror service: %w", err)
	}
	return nil
}

// applyFixups applies §7 exceptions 1 and 2 on the member plane. NotFound
// (nothing registered yet) is reported as notYet rather than an error.
func applyFixups(ctx context.Context, cs kubernetes.Interface, nodeName string) (notYet bool, err error) {
	if err := RewriteMemberAgentKubeconfig(ctx, cs); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("exception 1 (rewrite agent kubeconfig): %w", err)
	}
	if err := PinAgentToNode(ctx, cs, nodeName); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("exception 2 (pin karmada-agent to %s): %w", nodeName, err)
	}
	return false, nil
}
