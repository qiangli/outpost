package fleetreg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"strings"
)

// Placement names WHERE the control plane this host joined actually runs.
//
// It exists because placement became a user choice
// (dhnt/docs/dks-control-plane-on-sphere.md): the same host, running the same
// agent, may be attached to a cloudbox-hosted plane today and a peer-hosted
// one tomorrow. Reporting it is what makes that move VISIBLE — otherwise a
// node silently changes which cluster it belongs to and the only way to find
// out is to go read its config.
const (
	// PlacementSelf — this host hosts the apiserver.
	PlacementSelf = "self"
	// PlacementPeer — a control plane on another of the user's own machines,
	// reached through a tunnel that lands on loopback.
	PlacementPeer = "peer"
	// PlacementCloudbox — the hosted control plane.
	PlacementCloudbox = "cloudbox"
	// PlacementExternal — anything else: a rented always-on box, a managed
	// provider cluster.
	PlacementExternal = "external"
)

// ClusterInfo is what a host reports about its Kubernetes participation.
//
// It is deliberately a REPORT, not a definition: an outpost may say which
// cluster it is in, it may not author the cluster. That is the same
// report/author split the *:registry vs *:write scopes encode, and it is why
// this rides the existing fleet-registry push instead of getting an endpoint
// that could be mistaken for a control API.
type ClusterInfo struct {
	// Placement is one of the constants above.
	Placement string `json:"placement"`
	// Endpoint is the apiserver URL as THIS host reaches it. For a peer-hosted
	// plane that is a loopback address whose port is a local tunnel artifact —
	// meaningful for debugging this host, meaningless as a cluster identity,
	// which is precisely why it is not the identity.
	Endpoint string `json:"endpoint,omitempty"`
	// ControlPlane reports whether this host runs the apiserver.
	ControlPlane bool `json:"control_plane,omitempty"`
	// Nodes are the Kubernetes node names this host registers. One host owns
	// many nodes: one k3s agent plus one per virtual-kubelet backend.
	Nodes []string `json:"nodes,omitempty"`
	// Runtimes are the enabled backends (agent, vk-native, vk-podman, …).
	Runtimes []string `json:"runtimes,omitempty"`

	// HostsControlPlane reports that this host RUNS a control plane. It is a
	// fact about the host, deliberately separate from the row's own cluster:
	// hosting and joining became independent decisions, so a host can run a
	// plane for others while being a node of somebody else's.
	//
	// ControlPlane above stays the narrower claim — "this host hosts THE
	// CLUSTER IN THIS ROW" — because that is what the inventory needs to name
	// a cluster's control-plane member.
	HostsControlPlane bool `json:"hosts_control_plane,omitempty"`
	// ControlPlaneEndpoint is where that hosted plane's tunnel listens, when
	// this host runs one.
	ControlPlaneEndpoint string `json:"control_plane_endpoint,omitempty"`

	// CA is the cluster's TLS CA bundle. Never serialized — it is read only to
	// derive the cluster identity below.
	CA []byte `json:"-"`
	// NodeToken is the k3s join token. Never serialized — it is a CREDENTIAL,
	// read only for the CA hash it embeds.
	NodeToken string `json:"-"`
}

// ClusterID is the stable identity of the cluster this host joined.
//
// IT IS THE CA FINGERPRINT, NOT THE URL, and that choice is load-bearing. Every
// member of a cluster trusts the SAME apiserver CA, but members do not agree on
// a URL: a node reaching a peer-hosted plane through a tunnel sees
// `https://127.0.0.1:<locally-derived-port>`, and two nodes on one cluster
// derive DIFFERENT ports. Keying on the URL would split one cluster into one
// pseudo-cluster per node — the inventory would report N clusters of one node
// each and the grouping the page exists for would be silently wrong.
//
// The CA is a public certificate, so hashing it leaks nothing; the hash is used
// only so the identity is short and fixed-width.
//
// Falling back to the URL host is correct for the case that produces an empty
// CA — cloudbox fronting the apiserver behind a real, publicly-trusted cert —
// because there every member DOES agree on the URL.
// A JOIN TOKEN ALREADY CARRIES THE CLUSTER'S CA HASH, and it is the right
// source when joining a peer plane. k3s node tokens are
// `K10<sha256-of-cluster-CA>::<user>:<secret>`, so the identity is available
// even though the peer's CA bundle itself is not — a worker joining someone
// else's plane holds a token but no CA (its ClusterConfig.CA belongs to the
// cloudbox pairing, a DIFFERENT cluster). Reading it here is what stops a
// peer-joined host from being grouped under the cloudbox cluster whose CA it
// happens to be carrying.
func (c *ClusterInfo) ClusterID() string {
	if c == nil {
		return ""
	}
	if h := caHashFromNodeToken(c.NodeToken); h != "" {
		return "k8s-" + h[:12]
	}
	if pem := normalizePEM(c.CA); len(pem) > 0 {
		sum := sha256.Sum256(pem)
		return "k8s-" + hex.EncodeToString(sum[:])[:12]
	}
	if h := hostOf(c.Endpoint); h != "" {
		return h
	}
	return ""
}

// caHashFromNodeToken pulls the cluster CA hash out of a k3s node token.
// Returns "" for the short-form token (`K10::…` or a bare secret), which
// carries no hash — callers then fall back to the CA or the URL.
func caHashFromNodeToken(tok string) string {
	tok = strings.TrimSpace(tok)
	if !strings.HasPrefix(tok, "K10") {
		return ""
	}
	rest := tok[len("K10"):]
	i := strings.Index(rest, "::")
	if i <= 0 {
		return ""
	}
	h := rest[:i]
	// Must look like a sha256 hex digest; anything else is not an identity.
	if len(h) != 64 {
		return ""
	}
	for _, r := range h {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ""
		}
	}
	return h
}

// normalizePEM strips whitespace differences so two hosts holding the same CA
// with different trailing newlines still agree on the fingerprint.
func normalizePEM(pem []byte) []byte {
	return []byte(strings.Join(strings.Fields(string(pem)), "\n"))
}

// ClassifyPlacement decides where the joined control plane runs.
//
// Order matters: the control-plane host's own endpoint is loopback too, so the
// ControlPlane flag has to be consulted before the loopback test or every
// control plane would report itself as a peer.
//
// KNOWN LIMIT, stated rather than papered over: a host that joins a k3s it runs
// itself but never sets control_plane reports `peer`. The cluster IDENTITY is
// still correct (it comes from the CA), so grouping holds — only the placement
// label is wrong, and it is wrong because the config omitted the fact.
func ClassifyPlacement(apiURL, cloudboxURL string, controlPlane bool) string {
	if controlPlane {
		return PlacementSelf
	}
	h := hostOf(apiURL)
	if h == "" {
		return PlacementExternal
	}
	if isLoopbackHost(h) {
		return PlacementPeer
	}
	if cb := hostOf(cloudboxURL); cb != "" && strings.EqualFold(cb, h) {
		return PlacementCloudbox
	}
	return PlacementExternal
}

func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clusterAsset renders the report as one inventory row.
//
// Returns ok=false when there is nothing truthful to report — no cluster, or a
// cluster with no derivable identity. Reporting a nameless cluster would put a
// blank row on every host's page, which reads as "misconfigured" rather than
// "not participating".
func clusterAsset(c *ClusterInfo) (Asset, bool) {
	if c == nil {
		return Asset{}, false
	}
	id := c.ClusterID()
	if id == "" {
		return Asset{}, false
	}
	detail, err := json.Marshal(c)
	if err != nil {
		return Asset{}, false
	}
	return Asset{
		Kind:    "cluster",
		Name:    id,
		Display: clusterDisplay(c),
		Detail:  string(detail),
	}, true
}

// clusterDisplay is the human label. It names the PLACEMENT rather than the
// endpoint because the endpoint is either public and boring (cloudbox) or a
// local tunnel artifact (peer) — neither is what an operator scanning the page
// is looking for.
func clusterDisplay(c *ClusterInfo) string {
	switch c.Placement {
	case PlacementSelf:
		return "self-hosted (this host)"
	case PlacementPeer:
		return "peer-hosted"
	case PlacementCloudbox:
		if h := hostOf(c.Endpoint); h != "" {
			return "cloudbox (" + h + ")"
		}
		return "cloudbox"
	default:
		if h := hostOf(c.Endpoint); h != "" {
			return h
		}
		return "external"
	}
}
