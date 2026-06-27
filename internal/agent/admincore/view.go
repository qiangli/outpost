package admincore

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/clusterllm"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/otel"
	"github.com/qiangli/outpost/internal/agent/ycode"
)

// BuiltinView is the wire shape for one optional local-daemon proxy
// (podman/ollama). Enabled reflects the saved config; Available is the
// live detection result so the SPA can grey out the toggle when the
// daemon isn't running.
type BuiltinView struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Target    string `json:"target,omitempty"`
}

func toBuiltinView(enabled bool, bt agent.BuiltinTarget) BuiltinView {
	v := BuiltinView{Enabled: enabled, Available: bt.Available}
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
	Enabled       bool   `json:"enabled"`
	Mode          string `json:"mode,omitempty"`
	APIURL        string `json:"api_url,omitempty"`
	NodeName      string `json:"node_name,omitempty"`
	HasToken      bool   `json:"has_token"`
	HasCA         bool   `json:"has_ca"`
	HasNodeToken  bool   `json:"has_node_token,omitempty"`
	HasSTCPSecret bool   `json:"has_stcp_secret,omitempty"`
	K8sAPIPort    int    `json:"k8s_api_port,omitempty"`
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
	Enabled           bool   `json:"enabled"`
	Running           bool   `json:"running"`
	Installed         bool   `json:"installed"`
	StaleManifest     bool   `json:"stale_manifest"`
	PlatformSupported bool   `json:"platform_supported"`
	BinaryPath        string `json:"binary_path,omitempty"`
	APIEndpoint       string `json:"api_endpoint,omitempty"`
	Version           string `json:"version,omitempty"`
	DownloadURL       string `json:"download_url"`
}

// YcodeShareSurfaceView is one row in the SPA's ycode-share toggle
// list — the catalog entry's metadata plus the effective on/off
// state (after applying per-surface overlay against catalog default).
type YcodeShareSurfaceView struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Label     string `json:"label"`
	Enabled   bool   `json:"enabled"`
	DefaultOn bool   `json:"default_on"`
}

// toYcodeShareSurfacesView walks the catalog and folds the per-surface
// overlay against each entry's DefaultOn, producing the effective
// state the SPA renders. Always returns the full catalog so the UI
// can show every available toggle, including disabled ones.
func toYcodeShareSurfacesView(overlay map[string]bool) []YcodeShareSurfaceView {
	cat := otel.YcodeSurfaces()
	out := make([]YcodeShareSurfaceView, 0, len(cat))
	for _, s := range cat {
		out = append(out, YcodeShareSurfaceView{
			Name:      s.Name,
			Path:      s.Path,
			Label:     s.Label,
			Enabled:   otel.YcodeSurfaceEnabled(overlay, s.Name),
			DefaultOn: s.DefaultOn,
		})
	}
	return out
}

func toYcodeView(enabled bool, info ycode.Info) YcodeView {
	v := YcodeView{
		Enabled:           enabled,
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
	return ClusterView{
		Enabled:          fc.Cluster.Enabled,
		Mode:             fc.Cluster.Mode,
		APIURL:           fc.Cluster.APIURL,
		NodeName:         fc.ClusterNodeName(),
		HasToken:         fc.Cluster.Token != "",
		HasCA:            len(fc.Cluster.CA) > 0,
		HasNodeToken:     fc.Cluster.NodeToken != "",
		HasSTCPSecret:    fc.Cluster.STCPSecret != "",
		K8sAPIPort:       fc.Cluster.K8sAPIPort,
		MetricsRemoteURL: fc.Cluster.MetricsRemoteURL,
		LogsRemoteURL:    fc.Cluster.LogsRemoteURL,
		TracesRemoteURL:  fc.Cluster.TracesRemoteURL,
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
	ShellEnabled            bool             `json:"shell_enabled"`
	DesktopEnabled          bool             `json:"desktop_enabled"`
	ClipboardEnabled        bool             `json:"clipboard_enabled"`
	SSHEnabled              bool             `json:"ssh_enabled"`
	SSHAllowLocalForward    bool             `json:"ssh_allow_local_forward"`
	SSHAllowRemoteForward   bool             `json:"ssh_allow_remote_forward"`
	SSHAllowAgentForward    bool             `json:"ssh_allow_agent_forward"`
	SSHForwardSockets       []string         `json:"ssh_forward_sockets"`
	SFTPEnabled             bool             `json:"sftp_enabled"`
	FilesEnabled            bool             `json:"files_enabled"`
	FilesAllowWrite         bool             `json:"files_allow_write"`
	FilesScope              string           `json:"files_scope"`
	ClientOnly              bool             `json:"client_only"`
	Podman                  BuiltinView      `json:"podman"`
	Sandbox                 BuiltinView      `json:"sandbox"`
	Ollama                  BuiltinView      `json:"ollama"`
	OllamaPoolEnabled       bool             `json:"ollama_pool_enabled"`
	MeshEnabled             bool             `json:"mesh_enabled"`
	MeshPort                int              `json:"mesh_port,omitempty"`
	LoomEnabled             bool             `json:"loom_enabled"`
	LoomPort                int              `json:"loom_port,omitempty"`
	ZotEnabled              bool             `json:"zot_enabled"`
	ZotPort                 int              `json:"zot_port,omitempty"`
	SeaweedfsEnabled        bool             `json:"seaweedfs_enabled"`
	SeaweedfsPort           int              `json:"seaweedfs_port,omitempty"`
	OtelEnabled             bool             `json:"otel_enabled"`
	OtelPoolEnabled         bool             `json:"otel_pool_enabled"`
	Ycode                   YcodeView        `json:"ycode"`
	YcodeShareEnabled       bool             `json:"ycode_share_enabled"`
	YcodeShareRequireLogin  bool             `json:"ycode_share_require_login"`
	// YcodeShareSurfaces is the catalog rendered as effective state:
	// every entry the SPA might offer, with the boolean folding the
	// per-surface overlay against the catalog's DefaultOn. The SPA
	// renders one toggle row per entry; the value drives the switch.
	YcodeShareSurfaces []YcodeShareSurfaceView `json:"ycode_share_surfaces"`
	UpdateMode         string                  `json:"update_mode"`
	LLMPool            LLMPoolStatusView       `json:"llm_pool"`
	PeerTiers          []PeerTierView          `json:"peer_tiers,omitempty"`
	Mesh               *MeshStatusView         `json:"mesh,omitempty"`
	AppHealth          []AppHealthView         `json:"app_health,omitempty"`
	ClusterLLM         ClusterLLMView          `json:"cluster_llm"`
	Cluster            ClusterView             `json:"cluster"`
	Outbound           []agent.OutboundView    `json:"outbound"`
	Defaults           map[string]string       `json:"defaults"`
}

// SafeView returns the redacted view of the on-disk FileConfig + live
// state (built-in availability probes, outbound mount status, pool
// diagnostic). The Token / AccessToken / ProvisioningToken values are
// NEVER included — presence is reported via has_token only.
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
	return SafeView{
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
		FilesEnabled:            fc.FilesOn(),
		FilesAllowWrite:         fc.FilesAllowWrite,
		FilesScope:              fc.FilesScope,
		ClientOnly:              fc.ClientOnly,
		Podman:                  toBuiltinView(fc.PodmanOn(), s.detector.Podman()),
		// Sandbox shares the podman socket — availability is the podman
		// probe; enabled is its own toggle.
		Sandbox:                toBuiltinView(fc.SandboxOn(), s.detector.Podman()),
		Ollama:                 toBuiltinView(fc.OllamaOn(), s.detector.Ollama()),
		OllamaPoolEnabled:      fc.OllamaPoolOn(),
		MeshEnabled:            fc.MeshOn(),
		LoomEnabled:            fc.LoomOn(),
		LoomPort:               fc.LoomPort,
		ZotEnabled:             fc.ZotOn(),
		ZotPort:                fc.ZotPort,
		SeaweedfsEnabled:       fc.SeaweedfsOn(),
		SeaweedfsPort:          fc.SeaweedfsPort,
		MeshPort:               fc.MeshPort,
		OtelEnabled:            fc.OtelOn(),
		OtelPoolEnabled:        fc.OtelPoolOn(),
		Ycode:                  toYcodeView(fc.YcodeOn(), ycode.Detect()),
		YcodeShareEnabled:      fc.YcodeShareOn(),
		YcodeShareRequireLogin: fc.YcodeShareRequireLoginOn(),
		YcodeShareSurfaces:     toYcodeShareSurfacesView(fc.YcodeShareSurfaces),
		UpdateMode:             fc.UpdateModeName(),
		LLMPool:                s.llmPoolStatusView(fc),
		PeerTiers:              s.PeerTiers(),
		Mesh:                   s.MeshStatus(),
		AppHealth:              s.AppHealth(),
		ClusterLLM:             s.clusterLLMView(fc),
		Cluster:                toClusterView(fc),
		Outbound:               s.outboundList(),
		Defaults: map[string]string{
			"server_url": "https://ai.dhnt.io",
			"name":       defaultName,
			"os_user":    osUser,
			"local_addr": conf.DefaultLocalAddr,
			"vnc_addr":   conf.DefaultVNCAddr,
			"admin_addr": conf.DefaultAdminAddr,
		},
	}
}

// llmPoolStatusView returns the live pool diagnostic for the admin UI.
// When the pool isn't wired (LLMPoolStatus closure nil) or pool
// participation is off in the config, returns just {Enabled:false}.
func (s *Server) llmPoolStatusView(fc *conf.FileConfig) LLMPoolStatusView {
	if s.deps.LLMPoolStatus == nil {
		return LLMPoolStatusView{Enabled: fc.OllamaPoolOn()}
	}
	v := s.deps.LLMPoolStatus()
	v.Enabled = fc.OllamaPoolOn()
	return v
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
	Configured    bool            `json:"configured"`
	AgentName     string          `json:"agent_name,omitempty"`
	ServerAddr    string          `json:"server_addr,omitempty"`
	CloudboxURL   string          `json:"cloudbox_url,omitempty"`
	CurrentOSUser string          `json:"current_os_user,omitempty"`
	Build         agent.BuildInfo `json:"build"`
	BinaryPath    string          `json:"binary_path,omitempty"`
}

// Status returns the lightweight paired-yet payload.
func (s *Server) Status() (StatusView, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return StatusView{}, err
	}
	osUser, _ := hostauth.CurrentUser()
	exe, _ := os.Executable()
	return StatusView{
		Configured:    fc.AgentName != "",
		AgentName:     fc.AgentName,
		ServerAddr:    fc.ServerAddr,
		CloudboxURL:   CloudboxHTTPBase(fc),
		CurrentOSUser: osUser,
		Build:         agent.ReadBuildInfo(),
		BinaryPath:    exe,
	}, nil
}
