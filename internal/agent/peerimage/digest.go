package peerimage

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// digestPrefix is the only content-digest algorithm containerd emits for the
// image manifests we build.
const digestPrefix = "sha256:"

// ValidContentDigest reports whether s is a well-formed "sha256:<64 hex>".
// Anything else — empty, truncated, a bare hex string, a ref that happens to
// look like a digest — is NOT a digest and must not be treated as evidence.
func ValidContentDigest(s string) bool {
	if !strings.HasPrefix(s, digestPrefix) {
		return false
	}
	body := s[len(digestPrefix):]
	if len(body) != 64 {
		return false
	}
	_, err := hex.DecodeString(body)
	return err == nil
}

// CtrRuntime reads the resident content digest out of a node's k3s containerd
// by execing into the <node>-runtime container, the same path recipebuilder
// uses to load images. It deliberately asks containerd (`ctr images ls`) rather
// than podman: the podman-side tag is a build artifact that can be stale or
// retagged, while containerd's DIGEST column is the bytes the kubelet will run.
type CtrRuntime struct {
	// BashyBin is the resolved bashy executable.
	BashyBin string
	// BashyPath, when set, resolves the bashy executable lazily on each call
	// and takes precedence over BashyBin. The daemon wires this to its
	// self-healing resolver so constructing the service never blocks boot on
	// provisioning a missing userland — the first digest read resolves it.
	BashyPath func(ctx context.Context) (string, error)
	// Container is the <node-name>-runtime container hosting k3s containerd.
	Container string
	// Namespace is the containerd namespace; empty → k8s.io (what k3s uses
	// for images the kubelet can actually run).
	Namespace string
	// run is the command hook; nil → exec. Tests substitute it.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (c CtrRuntime) namespace() string {
	if strings.TrimSpace(c.Namespace) == "" {
		return "k8s.io"
	}
	return c.Namespace
}

func (c CtrRuntime) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if c.run != nil {
		return c.run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// Stderr is deliberately discarded rather than forwarded: this runs on a
	// host whose PTY capture is unredacted, and ctr/podman diagnostics can
	// echo registry credentials from the runtime's config. The exit status
	// plus our own message is all a caller needs.
	return cmd.Output()
}

// ResidentDigest returns the tri-state answer for ref.
//
// A failure to consult the runtime returns an error — never StateAbsent. That
// distinction is the whole point: an unreachable runtime must not read as "the
// image simply isn't there", which a caller could then satisfy by building.
func (c CtrRuntime) ResidentDigest(ctx context.Context, ref string) (DigestState, string, error) {
	if strings.TrimSpace(ref) == "" {
		return StateUnknown, "", fmt.Errorf("ref is required")
	}
	if strings.TrimSpace(c.Container) == "" {
		return StateUnknown, "", fmt.Errorf("runtime container is required")
	}
	bin := c.BashyBin
	if c.BashyPath != nil {
		b, err := c.BashyPath(ctx)
		if err != nil {
			// An unresolvable toolchain is an unreachable runtime: we learned
			// nothing about the image, so this is unknown — never absent.
			return StateUnknown, "", fmt.Errorf("resolve build toolchain: %w", err)
		}
		bin = b
	}
	if strings.TrimSpace(bin) == "" {
		return StateUnknown, "", fmt.Errorf("no build toolchain is configured")
	}
	out, err := c.exec(ctx, bin, "podman", "exec", c.Container,
		"k3s", "ctr", "-n", c.namespace(), "images", "ls", "name=="+ref)
	if err != nil {
		// Do NOT downgrade this to StateAbsent. We did not learn anything
		// about the image; we learned the runtime is not answering.
		return StateUnknown, "", fmt.Errorf("consult containerd in %s: %w", c.Container, err)
	}
	return parseCtrImagesLs(string(out), ref)
}

// parseCtrImagesLs extracts the DIGEST column for ref from `ctr images ls`
// tabular output. Split out from the exec so the parsing rules are unit-tested
// offline against real-shaped fixtures.
func parseCtrImagesLs(out, ref string) (DigestState, string, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "REF" { // header
			continue
		}
		if fields[0] != ref {
			continue
		}
		// REF TYPE DIGEST SIZE PLATFORMS LABELS — the ref exists. Whether we
		// can name its bytes is a separate question.
		if len(fields) < 3 {
			return StateUnknown, "", nil
		}
		got := fields[2]
		if !ValidContentDigest(got) {
			return StateUnknown, "", nil
		}
		return StateResident, got, nil
	}
	return StateAbsent, "", nil
}
