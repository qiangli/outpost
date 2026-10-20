package federation

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/external/registry"
)

// Runner executes karmadactl. A seam: tests substitute a fake and never need
// the binary (which has no Windows release and is download+exec, never
// bundled).
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ErrUnsupportedOS is returned on Windows, where karmadactl has no release.
var ErrUnsupportedOS = fmt.Errorf("federation is not supported on windows: karmadactl has no Windows release")

// CheckOS refuses federation verbs on Windows with a clear error.
func CheckOS() error { return CheckOSFor(runtime.GOOS) }

// ExecRunner runs the pinned, sha256-verified karmadactl from Yoke's managed
// registry. Ensure is a test seam; production callers leave it nil.
type ExecRunner struct {
	Ensure func(context.Context) (string, error)
}

// Resolve provisions the managed CLI on demand. Outpost can run independently
// of bashy's process, so the binary cannot be assumed to be on PATH.
func (r ExecRunner) Resolve(ctx context.Context) (string, error) {
	if r.Ensure != nil {
		return r.Ensure(ctx)
	}
	entry, ok := registry.Lookup("karmadactl")
	if !ok {
		return "", fmt.Errorf("karmadactl is not registered in the managed CLI catalog")
	}
	return entry.Ensure(ctx)
}

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := r.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("karmadactl %s: %w: %s", firstArg(args), err, strings.TrimSpace(redactTokens(out.String())))
	}
	return out.Bytes(), nil
}

func firstArg(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

var tokenFlag = regexp.MustCompile(`(--token[= ])\S+`)

func redactTokens(s string) string { return tokenFlag.ReplaceAllString(s, "${1}<redacted>") }

// LoopbackEndpoint is the host:port karmadactl dials: the federation port on
// host loopback (the publisher's forward on a host, the visitor on a member).
func LoopbackEndpoint(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// RegisterCommand is what `karmadactl token create --print-register-command`
// prints, parsed.
type RegisterCommand struct {
	Endpoint string
	Token    string
	CAHash   string // hex, no prefix
}

var (
	reToken = regexp.MustCompile(`--token[= ]([a-z0-9]{6}\.[a-z0-9]{16})`)
	reHash  = regexp.MustCompile(`--discovery-token-ca-cert-hash[= ]sha256:([0-9a-fA-F]{64})`)
	reReg   = regexp.MustCompile(`karmadactl\s+register\s+(\S+)`)
)

// ParseRegisterCommand extracts the bootstrap token, CA hash and endpoint.
func ParseRegisterCommand(out string) (RegisterCommand, error) {
	var rc RegisterCommand
	if m := reToken.FindStringSubmatch(out); m != nil {
		rc.Token = m[1]
	}
	if m := reHash.FindStringSubmatch(out); m != nil {
		rc.CAHash = strings.ToLower(m[1])
	}
	if m := reReg.FindStringSubmatch(out); m != nil {
		rc.Endpoint = m[1]
	}
	if rc.Token == "" || rc.CAHash == "" {
		return rc, fmt.Errorf("could not find a bootstrap token and CA hash in karmadactl output")
	}
	return rc, nil
}

// MintBootstrapToken runs `karmadactl token create` against the federation
// kubeconfig and parses the register command.
func MintBootstrapToken(ctx context.Context, r Runner, karmadaKubeconfig, ttl string) (RegisterCommand, error) {
	if ttl == "" {
		ttl = "24h"
	}
	out, err := r.Run(ctx, "token", "create", "--print-register-command", "--ttl", ttl, "--kubeconfig", karmadaKubeconfig)
	if err != nil {
		return RegisterCommand{}, err
	}
	return ParseRegisterCommand(string(out))
}

// RegisterSelf registers the host's own plane as a pull-mode member: mint a
// bootstrap token, run `karmadactl register` against the loopback endpoint with
// the plane's admin kubeconfig, then repoint the baked agent kubeconfig at the
// in-cluster Service name. Idempotent callers gate on the Cluster object.
func RegisterSelf(ctx context.Context, r Runner, karmadaKubeconfig, planeKubeconfig, clusterName string, port int, post func(context.Context) error) error {
	rc, err := MintBootstrapToken(ctx, r, karmadaKubeconfig, "1h")
	if err != nil {
		return err
	}
	if err := RegisterMember(ctx, r, LoopbackEndpoint(port), rc.Token, rc.CAHash, clusterName, planeKubeconfig); err != nil {
		return err
	}
	if post != nil {
		return post(ctx)
	}
	return nil
}
