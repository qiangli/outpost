package federation

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
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
func CheckOS() error {
	if runtime.GOOS == "windows" {
		return ErrUnsupportedOS
	}
	return nil
}

// ExecRunner runs a karmadactl binary: $OUTPOST_KARMADACTL, else PATH.
type ExecRunner struct{}

// Resolve returns the karmadactl path or an actionable error.
func (ExecRunner) Resolve() (string, error) {
	if p := strings.TrimSpace(os.Getenv("OUTPOST_KARMADACTL")); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("karmadactl")
	if err != nil {
		return "", fmt.Errorf("karmadactl not found: set OUTPOST_KARMADACTL or put it on PATH (`bashy dks federation` fetches the pinned release)")
	}
	return p, nil
}

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := r.Resolve()
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
	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	if _, err := r.Run(ctx, "register", endpoint,
		"--token", rc.Token,
		"--discovery-token-ca-cert-hash", "sha256:"+rc.CAHash,
		"--cluster-name", clusterName,
		"--kubeconfig", planeKubeconfig); err != nil {
		return err
	}
	if post != nil {
		return post(ctx)
	}
	return nil
}
