package admincore

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/clusterllm"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/otel"
	"github.com/qiangli/outpost/internal/agent/runtime"
	"github.com/qiangli/outpost/internal/agent/ycode"
)

// BuiltinView is the wire shape for one optional local-daemon proxy
// (podman/ollama). Enabled reflects the saved config; Available is the
// live detection result so the SPA can grey out the toggle when the
// daemon isn't running.
// BuiltinView is what the host HAS, not what is switched on: whether a
// local daemon was detected and where it answers.
//
// There is no Enabled field any more. Whether outpost proxies the daemon is
// a service unit's desired state (`services` below, joined with the
// monitor's view on ServiceStatus), and carrying a second copy here would
// be exactly the drift the unit model exists to remove. Detection stays,
// because "podman is not installed on this host" is the one thing a
// declaration can never tell an operator.
type BuiltinView struct {
	Available bool   `json:"available"`
	Target    string `json:"target,omitempty"`
}

func toBuiltinView(bt agent.BuiltinTarget) BuiltinView {
	v := BuiltinView{Available: bt.Available}
	switch bt.Scheme {
	case "unix", "npipe":
		if bt.Socket != "" {
			v.Target = bt.Scheme + "://" + bt.Socket
		}
	case "http", "https":
		v.Target = bt.URL
	}
	return v
}

// ClusterView is the redacted cluster status sent to UI / MCP callers.
// Token + CA bytes never leave the agent; presence is reported via
// has_token / has_ca.
type ClusterView struct {
	Enabled       bool     `json:"enabled"`
	Agent         bool     `json:"agent"`
	Virtual       []string `json:"virtual,omitempty"`
	APIURL        string   `json:"api_url,omitempty"`
	NodeName      string   `json:"node_name,omitempty"`
	HasToken      bool     `json:"has_token"`
	HasCA         bool     `json:"has_ca"`
	HasNodeToken  bool     `json:"has_node_token,omitempty"`
	HasSTCPSecret bool     `json:"has_stcp_secret,omitempty"`
	K8sAPIPort    int      `json:"k8s_api_port,omitempty"`
	// ControlPlane reports whether this host HOSTS the apiserver rather than
	// merely joining a cluster. Surfaced because it changes what the host is:
	// a control-plane host runs a tunnel server other machines depend on, so
	// restarting it is a cluster-wide event rather than a local one.
	//
	// NOT omitempty. This flag has a history of being dropped by config-write
	// paths (see mergePairing's inverted merge), and an omitempty bool renders
	// the dropped case and the never-hosted case identically as a missing key —
	// so `outpost status` answered "is this host the control plane?" with null
	// either way. An explicit false is the whole point of reporting it.
	ControlPlane bool `json:"control_plane"`
	// ControlPlaneKubeconfig is the path to the admin kubeconfig for the plane
	// this host HOSTS. Reported because it is the input the control-plane
	// reconcilers resolve at boot: an empty value means they declined to start,
	// which is directly observable here instead of only in the daemon log. It
	// is a path, not a credential — the file's contents never leave the host.
	ControlPlaneKubeconfig string `json:"control_plane_kubeconfig,omitempty"`
	TunnelBindAddr         string `json:"tunnel_bind_addr,omitempty"`
	TunnelBindPort         int    `json:"tunnel_bind_port,omitempty"`
	HasTunnelToken         bool   `json:"has_tunnel_token,omitempty"`
	TunnelLANExposed       bool   `json:"tunnel_lan_exposed,omitempty"`
	// JoinEndpoint names the PEER-hosted control plane this host joins, empty
	// when it joins the cloudbox-hosted one. The address is reportable; the
	// credential that goes with it is not, so only its presence appears here —
	// same treatment cluster.token and the tunnel token get.
	JoinEndpoint string `json:"join_endpoint,omitempty"`
	HasJoinToken bool   `json:"has_join_token,omitempty"`
	// HasCloudSTCPSecret reports whether cloudbox's own cluster STCP secret
	// is retained (cluster.cloud_stcp_secret) — the credential a
	// peer-joined worker's overlay-control relay needs to register on the
	// tailnet. Its absence is why a peer-flannel runtime refuses to start,
	// so the presence flag is what makes that refusal diagnosable from the
	// UI/MCP without reading daemon logs.
	HasCloudSTCPSecret bool `json:"has_cloud_stcp_secret,omitempty"`
	// PodNetworkMode is "overlay" (cloudbox allocated a per-node pod
	// CIDR), "peer-flannel" (a peer-hosted plane; stock flannel VXLAN
	// over the tailnet allocates from Node.spec.podCIDR — also
	// multi-node-correct, and PodCIDR is empty because this side does
	// not know it), or "single-node-fallback" (no CIDR: a fixed range
	// identical on every node, so pod IPs collide the moment a second
	// node joins). Read-only derived state,
	// not a config key — see runtime.ClassifyPodNetwork. PodCIDR is the
	// range that mode actually allocates from.
	PodNetworkMode string `json:"pod_network_mode,omitempty"`
	PodCIDR        string `json:"pod_cidr,omitempty"`
	// Observability fleet-aggregation URLs cloudbox provisioned for
	// this outpost. Empty when the AppStore observability bundle
	// isn't installed; non-empty means ycode is expected to
	// remote_write metrics / push logs / OTLP-export traces here
	// through the tailscale overlay.
	MetricsRemoteURL string `json:"metrics_remote_url,omitempty"`
	LogsRemoteURL    string `json:"logs_remote_url,omitempty"`
	TracesRemoteURL  string `json:"traces_remote_url,omitempty"`
}

// YcodeView is the redacted-and-flattened ycode status the admin
// UI / MCP API consume. Mirrors ycode.Info but flattens the State
// enum into named bools so the JS doesn't have to know the State
// vocabulary. Detection-only — outpost never spawns or restarts
// ycode itself.
type YcodeView struct {
	Running           bool   `json:"running"`
	Installed         bool   `json:"installed"`
	StaleManifest     bool   `json:"stale_manifest"`
	PlatformSupported bool   `json:"platform_supported"`
	BinaryPath        string `json:"binary_path,omitempty"`
	APIEndpoint       string `json:"api_endpoint,omitempty"`
	Version           string `json:"version,omitempty"`
	DownloadURL       string `json:"download_url"`
}

// YcodeShareSurfaceView is one entry of the ycode surface CATALOG: what
// this outpost build knows how to publish, and which entries it publishes
// when the unit's `settings.share` names none.
//
// It is no longer a toggle row. Which surfaces are published is one
// declared list (`settings.share` on the ycode unit), so the SPA offers the
// catalog and writes that list rather than a per-surface boolean map.
type YcodeShareSurfaceView struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Label     string `json:"label"`
	DefaultOn bool   `json:"default_on"`
}

// ycodeShareSurfacesView is the catalog this build can publish.
func ycodeShareSurfacesView() []YcodeShareSurfaceView {
	cat := otel.YcodeSurfaces()
	out := make([]YcodeShareSurfaceView, 0, len(cat))
	for _, s := range cat {
		out = append(out, YcodeShareSurfaceView{
			Name: s.Name, Path: s.Path, Label: s.Label, DefaultOn: s.DefaultOn,
		})
	}
	return out
}

func toYcodeView(info ycode.Info) YcodeView {
	v := YcodeView{
		PlatformSupported: info.PlatformSupported,
		BinaryPath:        info.BinaryPath,
		APIEndpoint:       info.APIEndpoint,
		Version:           info.Version,
		DownloadURL:       info.DownloadURL,
	}
	switch info.State {
	case ycode.StateRunning:
		v.Running = true
		v.Installed = true
	case ycode.StateInstalled:
		v.Installed = true
	case ycode.StateStaleManifest:
		v.StaleManifest = true
		// The binary may also be installed (locateBinary still ran);
		// the test for that lives on BinaryPath != "".
		if info.BinaryPath != "" {
			v.Installed = true
		}
	}
	return v
}

func toClusterView(fc *conf.FileConfig) ClusterView {
	if fc == nil || fc.Cluster == nil {
		return ClusterView{}
	}
	// A peer-joined worker gets its pod network from stock flannel over
	// the tailnet, so an empty OverlayPodCIDR does NOT mean the
	// single-node fallback there — reporting it as such would tell an
	// operator their multi-node peer cluster is about to collide pod IPs
	// when it is in fact correctly configured.
	podNet := runtime.ClassifyPodNetwork(fc.Cluster.OverlayPodCIDR, fc.Cluster.JoinsPeerPlane())
	cpBindAddr, cpBindPort := fc.Cluster.TunnelBind()
	return ClusterView{
		// Report the EFFECTIVE state (nil defaults on), not the raw
		// pointer — the status row / SPA badge should show what the
		// boot path will actually do.
		Enabled:                fc.ClusterOn(),
		PodNetworkMode:         string(podNet.Mode),
		PodCIDR:                podNet.PodCIDR,
		Agent:                  fc.Cluster.Runtimes.Agent,
		Virtual:                fc.Cluster.VirtualRuntimes(),
		APIURL:                 fc.Cluster.APIURL,
		NodeName:               fc.ClusterNodeName(),
		HasToken:               fc.Cluster.Token != "",
		HasCA:                  len(fc.Cluster.CA) > 0,
		HasNodeToken:           fc.Cluster.NodeToken != "",
		HasSTCPSecret:          fc.Cluster.STCPSecret != "",
		K8sAPIPort:             fc.Cluster.K8sAPIPort,
		ControlPlane:           fc.Cluster.ControlPlaneOn(),
		ControlPlaneKubeconfig: fc.Cluster.ControlPlaneKubeconfig,
		TunnelBindAddr:         cpBindAddr,
		TunnelBindPort:         cpBindPort,
		HasTunnelToken:         fc.Cluster.TunnelToken != "",
		TunnelLANExposed:       !isLoopbackIP(cpBindAddr),
		JoinEndpoint:           fc.Cluster.JoinEndpoint,
		HasJoinToken:           fc.Cluster.JoinToken != "",
		HasCloudSTCPSecret:     fc.Cluster.CloudSTCPSecret != "",
		MetricsRemoteURL:       fc.Cluster.MetricsRemoteURL,
		LogsRemoteURL:          fc.Cluster.LogsRemoteURL,
		TracesRemoteURL:        fc.Cluster.TracesRemoteURL,
	}
}

// SafeView is the redacted FileConfig sent over the API. Token never
// leaves the agent; presence is reported as has_token instead.
type SafeView struct {
	AgentName   string `json:"agent_name"`
	ServerAddr  string `json:"server_addr"`
	ServerPort  int    `json:"server_port"`
	CloudboxURL string `json:"cloudbox_url,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	RemotePort  int    `json:"remote_port"`
	AuthURL     string `json:"auth_url,omitempty"`
	HasToken    bool   `json:"has_token"`
	LocalAddr   string `json:"local_addr,omitempty"`
	VNCAddr     string `json:"vnc_addr,omitempty"`
	AdminAddr   string `json:"admin_addr,omitempty"`
	// Wave 3A discovery + LAN-direct knobs (all default off).
	DiscoveryEnabled        bool             `json:"discovery_enabled"`
	SSHListenAddr           string           `json:"ssh_listen_addr,omitempty"`
	DiscoveryHTTPListenAddr string           `json:"discovery_http_listen_addr,omitempty"`
	PeerTrustPolicy         string           `json:"peer_trust_policy,omitempty"`
	AssignedHostname        string           `json:"assigned_hostname,omitempty"`
	OAuth2Email             string           `json:"oauth2_email,omitempty"`
	AdminUsers              []string         `json:"admin_users"`
	Apps                    []conf.AppConfig `json:"apps"`
	AppSecrets              []SecretPresence `json:"app_secrets,omitempty"`
	ShellEnabled            bool             `json:"shell_enabled"`
	DesktopEnabled          bool             `json:"desktop_enabled"`
	ClipboardEnabled        bool             `json:"clipboard_enabled"`
	SSHEnabled              bool             `json:"ssh_enabled"`
	SSHAllowLocalForward    bool             `json:"ssh_allow_local_forward"`
	SSHAllowRemoteForward   bool             `json:"ssh_allow_remote_forward"`
	SSHAllowAgentForward    bool             `json:"ssh_allow_agent_forward"`
	SSHForwardSockets       []string         `json:"ssh_forward_sockets"`
	SFTPEnabled             bool             `json:"sftp_enabled"`
	ClientOnly              bool             `json:"client_only"`
	// Podman and Ollama report DETECTION only — whether the daemon is
	// installed and reachable on this host. Whether outpost proxies it is
	// the matching service unit's desired state, in Services below.
	Podman      BuiltinView `json:"podman"`
	Ollama      BuiltinView `json:"ollama"`
	WarmDesired []string    `json:"warm_desired,omitempty"`
	MeshEnabled bool        `json:"mesh_enabled"`
	MeshPort    int         `json:"mesh_port,omitempty"`
	// Services is every declared service unit and its desired state. It
	// replaces the hand-maintained `bashy_services`, `bashy_apps_enabled`,
	// `loom_enabled` and `meet_enabled` rows: a new daemon is a declaration,
	// not three more fields here. Runtime state lives on ServiceStatus,
	// which reads the monitor.
	Services []ServiceUnitView `json:"services,omitempty"`
	// ServiceSecrets reports WHICH app routes hold a trusted-identity HMAC
	// key, never the keys themselves.
	ServiceSecrets []SecretPresence `json:"service_secrets,omitempty"`
	// serviceSSO is the unredacted key map, carried unexported so it can
	// never be marshalled: Redact turns it into ServiceSecrets and clears it.
	serviceSSO      map[string]string
	BashyVersion    string    `json:"bashy_version,omitempty"`
	CloudDOEnabled  bool      `json:"cloud_do_enabled"`
	HasCloudDOToken bool      `json:"has_cloud_do_token"`
	Ycode           YcodeView `json:"ycode"`
	// YcodeShareSurfaces is the CATALOG this build can publish, for the SPA
	// to offer when writing the ycode unit's `settings.share` list.
	YcodeShareSurfaces []YcodeShareSurfaceView `json:"ycode_share_surfaces"`
	UpdateMode         string                  `json:"update_mode"`
	LLMPool            LLMPoolStatusView       `json:"llm_pool"`
	PeerTiers          []PeerTierView          `json:"peer_tiers,omitempty"`
	Mesh               *MeshStatusView         `json:"mesh,omitempty"`
	AppHealth          []AppHealthView         `json:"app_health,omitempty"`
	ClusterLLM         ClusterLLMView          `json:"cluster_llm"`
	Cluster            ClusterView             `json:"cluster"`
	ControlPlane       *ControlPlaneStatus     `json:"control_plane,omitempty"`
	Outbound           []agent.OutboundView    `json:"outbound"`
	Defaults           map[string]string       `json:"defaults"`
}

// SafeView returns the redacted view of the on-disk FileConfig + live
// state (built-in availability probes, outbound mount status, pool
// diagnostic). Top-level secrets (Token, AccessToken, cloud/cluster keys) are
// NEVER included — presence is reported via has_* flags. Nested per-app and
// per-service credentials are still present for the interactive admin
// session; every other caller must use Redacted().
func (s *Server) SafeView() (SafeView, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return SafeView{}, err
	}
	return s.toSafeView(fc), nil
}

func (s *Server) toSafeView(fc *conf.FileConfig) SafeView {
	apps := fc.Apps
	if apps == nil {
		apps = []conf.AppConfig{}
	}
	osUser, _ := hostauth.CurrentUser()
	osHost, _ := os.Hostname()
	defaultName := osHost
	if osHost != "" && osUser != "" {
		defaultName = osHost + "-" + osUser
	}
	admins := fc.AdminUsers
	if admins == nil {
		admins = []string{}
	}
	sshSockets := fc.SSHForwardSockets
	if sshSockets == nil {
		sshSockets = []string{}
	}
	view := SafeView{
		AgentName:   fc.AgentName,
		ServerAddr:  fc.ServerAddr,
		ServerPort:  fc.ServerPort,
		CloudboxURL: CloudboxHTTPBase(fc),
		Protocol:    fc.Protocol,
		RemotePort:  fc.RemotePort,
		AuthURL:     fc.AuthURL,
		HasToken:    fc.Token != "",
		LocalAddr:   fc.LocalAddr,
		VNCAddr:     fc.VNCAddr,
		AdminAddr:   fc.AdminAddr,
		AdminUsers:  admins,
		// Wave 3A discovery + LAN-direct view.
		DiscoveryEnabled:        fc.DiscoveryOn(),
		SSHListenAddr:           fc.SSHListenAddr,
		DiscoveryHTTPListenAddr: fc.DiscoveryHTTPListenAddr,
		PeerTrustPolicy:         fc.EffectivePeerTrustPolicy(),
		AssignedHostname:        fc.AssignedHostname,
		OAuth2Email:             fc.OAuth2Email,
		Apps:                    apps,
		ShellEnabled:            fc.ShellOn(),
		DesktopEnabled:          fc.DesktopOn(),
		ClipboardEnabled:        fc.ClipboardOn(),
		SSHEnabled:              fc.SSHOn(),
		SSHAllowLocalForward:    fc.SSHAllowLocalForwardOn(),
		SSHAllowRemoteForward:   fc.SSHAllowRemoteForwardOn(),
		SSHAllowAgentForward:    fc.SSHAllowAgentForwardOn(),
		SSHForwardSockets:       sshSockets,
		SFTPEnabled:             fc.SFTPOn(),
		ClientOnly:              fc.ClientOnly,
		Podman:                  toBuiltinView(s.detector.Podman()),
		Ollama:                  toBuiltinView(s.detector.Ollama()),
		WarmDesired:             fc.WarmDesired,
		MeshEnabled:             fc.MeshOn(),
		Services:                serviceUnitViews(s.deps.Services),
		serviceSSO:              fc.ServiceSSO,
		BashyVersion:            fc.BashyVersion,
		CloudDOEnabled:          fc.CloudDOOn(),
		HasCloudDOToken:         fc.CloudDOToken != "",
		MeshPort:                fc.MeshPort,
		Ycode:                   toYcodeView(ycode.Detect()),
		YcodeShareSurfaces:      ycodeShareSurfacesView(),
		UpdateMode:              fc.UpdateModeName(),
		LLMPool:                 s.llmPoolStatusView(fc),
		PeerTiers:               s.PeerTiers(),
		Mesh:                    s.MeshStatus(),
		AppHealth:               s.AppHealth(),
		ClusterLLM:              s.clusterLLMView(fc),
		Cluster:                 toClusterView(fc),
		Outbound:                s.outboundList(),
		Defaults: map[string]string{
			"server_url": "https://ai.dhnt.io",
			"name":       defaultName,
			"os_user":    osUser,
			"local_addr": conf.DefaultLocalAddr,
			"vnc_addr":   conf.DefaultVNCAddr,
			"admin_addr": conf.DefaultAdminAddr,
		},
	}
	// Populate control-plane status. This is populated separately because
	// it's a read-only probe, not part of the config.
	if cpStatus, err := s.ControlPlaneStatusView(context.Background()); err == nil {
		view.ControlPlane = &cpStatus
	}
	return view
}

// llmPoolStatusView returns the live pool diagnostic for the admin UI.
//
// Enabled now comes from the LIVE capability, not from a config key: the
// closure reports the ollama unit's actual state, so the diagnostic can no
// longer say "enabled" about a pool whose unit is disabled or whose daemon
// went away. An unwired closure (test paths) reports the zero view.
func (s *Server) llmPoolStatusView(*conf.FileConfig) LLMPoolStatusView {
	if s.deps.LLMPoolStatus == nil {
		return LLMPoolStatusView{}
	}
	return s.deps.LLMPoolStatus()
}

// ClusterLLMView is the operator-facing snapshot of the intra-home
// distributed-inference backend (GPUStack first). State is one of
// clusterllm's StateUnconfigured / Running / NotReachable. HasAPIKey
// reflects whether a management key is set (the secret itself is never
// surfaced); without it AggregateVRAMBytes stays 0 and the cloudbox size
// filter is inert. MemberCount / AggregateVRAMBytes are the live cluster
// shape the registry push advertises.
type ClusterLLMView struct {
	Configured         bool   `json:"configured"`
	Backend            string `json:"backend,omitempty"`
	State              string `json:"state"`
	Endpoint           string `json:"endpoint,omitempty"`
	Version            string `json:"version,omitempty"`
	HasAPIKey          bool   `json:"has_api_key"`
	MemberCount        int    `json:"member_count,omitempty"`
	AggregateVRAMBytes uint64 `json:"aggregate_vram_bytes,omitempty"`
}

// clusterLLMView builds the ClusterLLMView, performing a (TTL-cached)
// live probe of the configured backend. Unconfigured endpoints never
// touch the network. The detector is rebuilt when the endpoint/key
// changes so a save-then-poll between the restart sees fresh config.
func (s *Server) clusterLLMView(fc *conf.FileConfig) ClusterLLMView {
	endpoint := strings.TrimSpace(fc.ClusterLLMEndpoint)
	if endpoint == "" {
		return ClusterLLMView{State: string(clusterllm.StateUnconfigured)}
	}
	key := endpoint + "\x00" + fc.ClusterLLMAPIKey
	s.clusterMu.Lock()
	if s.clusterDet == nil || s.clusterKey != key {
		s.clusterDet = clusterllm.NewDetector(clusterllm.Config{
			Endpoint: endpoint,
			APIKey:   fc.ClusterLLMAPIKey,
		}, 5*time.Second, nil)
		s.clusterKey = key
	}
	det := s.clusterDet
	s.clusterMu.Unlock()
	info := det.Info(context.Background())
	return ClusterLLMView{
		Configured:         true,
		Backend:            info.Backend,
		State:              string(info.State),
		Endpoint:           info.Endpoint,
		Version:            info.Version,
		HasAPIKey:          fc.ClusterLLMAPIKey != "",
		MemberCount:        info.MemberCount,
		AggregateVRAMBytes: info.AggregateVRAMBytes,
	}
}

// outboundList safely returns the outbound manager's view list (or an
// empty slice when no manager is wired, so JSON serialization never
// emits a null field).
func (s *Server) outboundList() []agent.OutboundView {
	if s.deps.Outbound == nil {
		return []agent.OutboundView{}
	}
	return s.deps.Outbound.List()
}

// StatusView is the small "is outpost paired yet?" shape the SPA polls
// to decide what to render. Mirrors the legacy /api/status payload.
//
// Build + BinaryPath are added so a remote operator (e.g. `outpost
// upgrade` running on another box that drives this daemon over MCP, or
// cloudbox's fleet view) can see the running daemon's provenance and
// the path of the binary to swap on disk.
type StatusView struct {
	Configured          bool            `json:"configured"`
	AgentName           string          `json:"agent_name,omitempty"`
	ServerAddr          string          `json:"server_addr,omitempty"`
	CloudboxURL         string          `json:"cloudbox_url,omitempty"`
	CurrentOSUser       string          `json:"current_os_user,omitempty"`
	Build               agent.BuildInfo `json:"build"`
	BinaryPath          string          `json:"binary_path,omitempty"`
	ControlPlaneSummary string          `json:"control_plane_summary,omitempty"`
	// ControlPlaneState is "failed" with ControlPlaneFailure when a hosted
	// plane has no runtime behind it.
	ControlPlaneState   string `json:"control_plane_state,omitempty"`
	ControlPlaneFailure string `json:"control_plane_failure,omitempty"`
	// Cluster is the worker-side plane row; nil when cluster is off.
	Cluster *ClusterPlaneView `json:"cluster,omitempty"`
}

// ClusterPlaneView reports whether the plane this host joined is reachable.
// Endpoint carries no credentials.
type ClusterPlaneView struct {
	Joined       bool      `json:"joined"`
	Endpoint     string    `json:"endpoint,omitempty"`
	APIReachable string    `json:"apiserver_reachable"` // yes | no | unknown
	LastSuccess  time.Time `json:"last_success,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
}

// BuildClusterPlaneView joins config with the runtime's plane-health snapshot.
func BuildClusterPlaneView(fc *conf.FileConfig, h runtime.PlaneHealth, now time.Time) *ClusterPlaneView {
	if fc == nil || !fc.ClusterOn() || fc.Cluster == nil {
		return nil
	}
	ep := fc.Cluster.JoinEndpoint
	if ep == "" {
		ep = fc.Cluster.APIURL
	}
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		ep = u.String()
	}
	v := &ClusterPlaneView{Joined: true, Endpoint: ep, APIReachable: h.Reachable(now), LastSuccess: h.LastSuccess}
	if v.APIReachable != "yes" {
		v.LastError = h.LastError
	}
	return v
}

// Status returns the lightweight paired-yet payload.
func (s *Server) Status() (StatusView, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return StatusView{}, err
	}
	osUser, _ := hostauth.CurrentUser()
	exe, _ := os.Executable()
	view := StatusView{
		Configured:    fc.AgentName != "",
		AgentName:     fc.AgentName,
		ServerAddr:    fc.ServerAddr,
		CloudboxURL:   CloudboxHTTPBase(fc),
		CurrentOSUser: osUser,
		Build:         agent.ReadBuildInfo(),
		BinaryPath:    exe,
	}
	// Add control-plane status summary if available.
	if cpStatus, err := s.ControlPlaneStatusView(context.Background()); err == nil && cpStatus.Hosted {
		view.ControlPlaneSummary = cpStatusSummary(&cpStatus)
		if cpStatus.State == "failed" {
			view.ControlPlaneState, view.ControlPlaneFailure = cpStatus.State, cpStatus.FailureReason
		}
	}
	view.Cluster = BuildClusterPlaneView(fc, runtime.LastPlaneHealth(), time.Now())
	return view, nil
}

// cpStatusSummary generates a one-line summary of control-plane status.
func cpStatusSummary(status *ControlPlaneStatus) string {
	if !status.Hosted {
		return ""
	}
	parts := []string{}
	if status.State == "failed" {
		return "FAILED: " + status.FailureReason
	}
	if status.ContainerRunning {
		parts = append(parts, "running")
		if status.APIServerServing {
			parts = append(parts, fmt.Sprintf("serving (%d nodes)", status.NodeCount))
		}
	} else if status.ContainerExists {
		parts = append(parts, "stopped")
	} else {
		parts = append(parts, "not started")
	}
	if status.JoinEndpoint != "" {
		parts = append(parts, "join enabled")
	}
	if len(parts) == 0 {
		return "no status"
	}
	return strings.Join(parts, ", ")
}
