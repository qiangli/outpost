package runtime

import (
	"bytes"
	"io/fs"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func embeddedEntrypoint(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(imageFS, "image/entrypoint.sh")
	if err != nil {
		t.Fatalf("read embedded entrypoint: %v", err)
	}
	return string(data)
}

// The Go constant and the shell default are two spellings of one value,
// and they drifted before: podnet.go said 10.43.42.0/24 — inside the k3s
// SERVICE CIDR, the silent node-local misroute the entrypoint comment
// records as fixed — long after the entrypoint moved to 10.42.255.0/24.
// Nothing failed; the status view and the boot WARN simply reported a
// range no container ever allocated from. This test is the pin.
func TestFallbackPodCIDRMatchesEntrypoint(t *testing.T) {
	script := embeddedEntrypoint(t)
	re := regexp.MustCompile(`CNI_LOCAL_POD_CIDR="\$\{CNI_LOCAL_POD_CIDR:-([^}]+)\}"`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatal("entrypoint no longer defines a CNI_LOCAL_POD_CIDR default; " +
			"update this pin along with FallbackPodCIDR")
	}
	if got := m[1]; got != FallbackPodCIDR {
		t.Fatalf("fallback pod CIDR drifted: entrypoint=%q FallbackPodCIDR=%q "+
			"(the entrypoint owns the value; correct the Go constant)", got, FallbackPodCIDR)
	}
	// The historical bug, pinned by name so a revert is loud.
	if strings.HasPrefix(FallbackPodCIDR, "10.43.") {
		t.Fatalf("FallbackPodCIDR %q is inside the k3s service CIDR (10.43.0.0/16); "+
			"a ClusterIP allocated into it resolves to a local pod and kube-proxy "+
			"never sees the packet", FallbackPodCIDR)
	}
}

func TestPodNetworkEnvArgs(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want []string
	}{
		{
			// Managed/cloud path: nothing new on the command line.
			name: "cloudbox overlay stamps no mode env",
			opts: Options{PodCIDR: "10.42.7.0/24", OverlayLoginServer: "https://cb", OverlayAuthKey: "k"},
			want: nil,
		},
		{
			name: "single-node fallback stamps no mode env",
			opts: Options{},
			want: nil,
		},
		{
			name: "peer flannel stamps the mode env",
			opts: Options{PeerFlannel: true},
			want: []string{"-e", "OUTPOST_POD_NETWORK_MODE=peer-flannel"},
		},
		{
			name: "peer flannel with a stale cloudbox cidr still stamps peer-flannel",
			opts: Options{PeerFlannel: true, PodCIDR: "10.42.7.0/24"},
			want: []string{"-e", "OUTPOST_POD_NETWORK_MODE=peer-flannel"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podNetworkEnvArgs(tt.opts)
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Fatalf("podNetworkEnvArgs = %q, want %q", got, tt.want)
			}
		})
	}
}

// The env var name is a wire contract with the embedded shell script; a
// rename on either side alone silently disables the whole mode.
func TestPodNetworkModeEnvIsReadByEntrypoint(t *testing.T) {
	script := embeddedEntrypoint(t)
	if !strings.Contains(script, `: "${`+PodNetworkModeEnv+`:=}"`) {
		t.Fatalf("entrypoint does not default %s; the Go side stamps it", PodNetworkModeEnv)
	}
	if !strings.Contains(script, `"${`+PodNetworkModeEnv+`}" = "`+string(PodNetworkPeerFlannel)+`"`) {
		t.Fatalf("entrypoint does not branch on %s == %q", PodNetworkModeEnv, PodNetworkPeerFlannel)
	}
}

// (c) — the peer-flannel branch of the entrypoint, asserted property by
// property. Each of these is a silent failure if it regresses: a leftover
// conflist out-selects flannel by CNI's lexical pick, a written conflist
// does the same, and an addressless tailscale0 produces a node that joins,
// reports Ready, and drops every pod packet.
func TestEntrypointPeerFlannelBranch(t *testing.T) {
	script := embeddedEntrypoint(t)

	branchAt := strings.Index(script, `if [ "${OUTPOST_POD_NETWORK_MODE}" = "peer-flannel" ]; then`)
	if branchAt < 0 {
		t.Fatal("no peer-flannel branch in the CNI conflist block")
	}
	elifAt := strings.Index(script[branchAt:], `elif [ -n "${OUTPOST_POD_CIDR}" ]; then`)
	if elifAt < 0 {
		t.Fatal("the peer-flannel branch does not precede the overlay conflist branch; " +
			"an overlay CIDR left behind by a previous mode would win")
	}
	branch := script[branchAt : branchAt+elifAt]

	// Removes BOTH stale conflists.
	if !strings.Contains(branch, "rm -f /etc/cni/net.d/10-outpost.conflist /etc/cni/net.d/10-bridge.conflist") {
		t.Error("peer-flannel branch does not remove both stale conflists")
	}
	// Writes NONE.
	if strings.Contains(branch, "cat > /etc/cni/net.d/") {
		t.Error("peer-flannel branch writes a conflist; flannel must write its own")
	}

	// Waits for a real tailscale IPv4 before the agent starts.
	k3sAt := strings.LastIndex(script, "exec /usr/local/bin/k3s agent")
	if k3sAt < 0 {
		t.Fatal("entrypoint no longer execs k3s agent")
	}
	waitAt := strings.Index(script, `K3S_FLANNEL_ARGS=""`)
	if waitAt < 0 || waitAt > k3sAt {
		t.Fatal("no pre-k3s peer-flannel gate")
	}
	gate := script[waitAt:k3sAt]
	if !strings.Contains(gate, "tailscale") || !strings.Contains(gate, "ip -4") {
		t.Error("the gate does not poll for a tailscale IPv4 address")
	}
	// A timeout must FAIL, not continue — the opposite of the optional
	// overlay path above it, which deliberately continues to k3s.
	if !strings.Contains(gate, "exit 1") {
		t.Error("the gate does not exit non-zero on timeout; it would start a dark node")
	}
	if !strings.Contains(gate, `K3S_FLANNEL_ARGS="--flannel-iface=tailscale0"`) {
		t.Error("the gate does not set --flannel-iface=tailscale0")
	}

	// ...and the argv actually carries it.
	if !strings.Contains(script[k3sAt:], "${K3S_FLANNEL_ARGS}") {
		t.Error("k3s agent argv does not include K3S_FLANNEL_ARGS")
	}
	// --node-ip is a live-test question, not a requirement (see the ADR).
	// Pinned so it cannot be added on a guess.
	if strings.Contains(script[k3sAt:], "--node-ip") {
		t.Error("--node-ip appeared in the k3s agent argv; the ADR records it as " +
			"unresolved pending a two-machine test, not as a requirement")
	}
}

// (d) — the managed/cloud path is untouched. Both pre-existing branches
// still write their conflists, unguarded by the new mode.
func TestEntrypointManagedPathUnchanged(t *testing.T) {
	script := embeddedEntrypoint(t)
	for _, want := range []string{
		`elif [ -n "${OUTPOST_POD_CIDR}" ]; then`,
		"cat > /etc/cni/net.d/10-outpost.conflist",
		"cat > /etc/cni/net.d/10-bridge.conflist",
		`"type": "outpost-cni"`,
		`--advertise-routes=${OUTPOST_POD_CIDR}`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("managed/cloud path lost %q", want)
		}
	}
	// The overlay branch must still be reachable with no mode env set,
	// which is exactly what a cloudbox-hosted node has.
	if strings.Contains(script, `if [ -n "${OUTPOST_POD_CIDR}" ] && [ "${OUTPOST_POD_NETWORK_MODE}"`) {
		t.Error("the overlay branch was made conditional on the new mode env")
	}
}

// Peer-flannel must not borrow the fallback's catastrophe warning.
func TestPeerFlannelLogIsNotTheFallbackWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ClassifyPodNetwork("", true).Log("node-a")

	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("peer-flannel logged a warning: %s", out)
	}
	for _, forbidden := range []string{"NO pod network", "single-node fallback", "re-pair"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("peer-flannel log reuses the fallback warning text %q: %s", forbidden, out)
		}
	}
}
