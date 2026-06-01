package conf

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileConfig is what the register command writes and `start` reads from
// disk. It pins everything the agent needs to dial the cloud — no more
// env juggling once registration has completed.
//
// AuthURL, when non-empty, switches the agent's /auth handler from the
// host OS (PAM / dscl / LogonUserW) to an external HTTP endpoint that
// owns its own application-level user list.
type FileConfig struct {
	AgentName  string `json:"agent_name"`
	ServerAddr string `json:"server_addr"`
	ServerPort int    `json:"server_port"`
	// Protocol is "tcp" (default for legacy raw-TCP matrix-tunnel
	// deploys), "ws", or "wss". Returned by /api/register/exchange so
	// the outpost knows how cloudbox expects to be dialed. Empty == "tcp".
	Protocol   string `json:"protocol,omitempty"`
	Token      string `json:"token"`
	RemotePort int    `json:"remote_port"`
	AuthURL    string `json:"auth_url,omitempty"`

	// AccessToken is the per-outpost scoped JWT cloudbox issues at
	// register time. Bearer-auth credential for /h/:host/ssh (used by
	// `outpost ssh-proxy`) and /api/v1/ssh/* (used by `outpost
	// ssh-config`). Distinct from Token, which is the *matrix-tunnel*
	// shared secret used by the FRP client.
	AccessToken string `json:"access_token,omitempty"`

	// ClientOnly marks this machine as a credential vehicle that should
	// never accept inbound traffic — the user wants to ssh OUT to other
	// paired hosts but not BE one. When true: `outpost start` skips
	// NewTunnel + the local gin server, /apps/etc. don't bind, and the
	// admin UI is the only loopback listener (for managing this row).
	ClientOnly bool `json:"client_only,omitempty"`

	// Apps managed through the admin UI. When this field is present (even
	// empty), it is authoritative — the legacy MATRIX_APPS env is ignored.
	// When absent (nil) on a config written before the admin UI shipped,
	// `start` falls back to MATRIX_APPS for back-compat.
	Apps []AppConfig `json:"apps,omitempty"`

	// LocalAddr is the local-loopback bind for the main HTTP server
	// (the one cloudbox reaches through the matrix tunnel). Default
	// "127.0.0.1:0" — random port. Persist a fixed port here if the
	// operator wants stable reverse-proxy rules or audit hooks pointed
	// at the matrix-tunnel ingress.
	LocalAddr string `json:"local_addr,omitempty"`

	// VNCAddr is the upstream the built-in /desktop route bridges to.
	// Default "127.0.0.1:5900" — the standard VNC port. Persist a
	// non-default value when the VNC daemon lives elsewhere.
	VNCAddr string `json:"vnc_addr,omitempty"`

	// AdminAddr is the loopback (or LAN) bind for the admin UI + MCP
	// server. Default "127.0.0.1:17777". Override here, via the
	// $OUTPOST_ADMIN_ADDR env var, or via the --admin-addr CLI flag;
	// the precedence is CLI flag > env > file > default. LAN binds
	// (0.0.0.0:17777) log a warning and force the auth gate on every
	// request — see adminui.requireSession.
	AdminAddr string `json:"admin_addr,omitempty"`

	// AdminUsers is an optional allowlist of OAuth-identified emails
	// who should be treated as admin when authenticating via the host
	// OS path. Empty list = the legacy "anyone who can prove the OS
	// password is admin" behavior. Non-empty = only listed emails get
	// admin; others get user. Ignored when AuthURL is set (the
	// external endpoint owns role assignment then). Was previously
	// reachable only as $MATRIX_ADMIN_USERS.
	AdminUsers []string `json:"admin_users,omitempty"`

	// Built-in route toggles. Pointer-bool so a missing field on an old
	// config means "default on", which matches the pre-admin-UI behavior.
	// Use ShellOn()/DesktopOn()/ClipboardOn()/SSHOn() to read; never deref directly.
	ShellEnabled     *bool `json:"shell_enabled,omitempty"`
	DesktopEnabled   *bool `json:"desktop_enabled,omitempty"`
	ClipboardEnabled *bool `json:"clipboard_enabled,omitempty"`
	SSHEnabled       *bool `json:"ssh_enabled,omitempty"`

	// SSHAllowLocalForward gates whether the built-in /ssh server accepts
	// `direct-tcpip` channels — the primitive behind stock `ssh -L` /
	// `ssh -D`. Default-on (matches pre-toggle behavior was rejection;
	// flipping the default to "on" is the whole point of adding this
	// switch). Loopback-only destinations regardless of this flag — see
	// the agent ssh.go `allowDirectTCPIPDest` allowlist. Disable here
	// (admin UI / JSON) to refuse the channel entirely.
	SSHAllowLocalForward *bool `json:"ssh_allow_local_forward,omitempty"`

	// SSHAllowRemoteForward gates whether the built-in /ssh server honors
	// `tcpip-forward` global requests — the primitive behind stock
	// `ssh -R`. Default-on. Bind address is loopback-only regardless of
	// this flag (see ssh.go `allowTCPIPForwardBind`); the operator who
	// can pass the OS-password gate already has equivalent reach via a
	// session-channel `nc` invocation, so adding this adds no authority.
	SSHAllowRemoteForward *bool `json:"ssh_allow_remote_forward,omitempty"`

	// SSHAllowAgentForward gates whether the built-in /ssh server accepts
	// `auth-agent-req@openssh.com` channel requests — the primitive
	// behind `ssh -A`. Default-on. When enabled, the server creates a
	// per-session Unix socket and sets SSH_AUTH_SOCK in the runner env;
	// agent traffic is byte-bridged back to the client via
	// `auth-agent@openssh.com` channels. Trust model: the SSH-auth-agent
	// protocol is opaque to the bridge, so the agent can only sign with
	// keys the client's local ssh-agent already trusts to sign. Disable
	// here to refuse the channel-request entirely.
	SSHAllowAgentForward *bool `json:"ssh_allow_agent_forward,omitempty"`

	// SFTPEnabled gates whether the embedded SSH server accepts the
	// "sftp" subsystem channel. Default-on: modern openssh `scp` (8.8+)
	// uses sftp under the hood, so leaving this off breaks scp for new
	// clients. Disable explicitly if you want to force legacy `scp -O`
	// (slower, rides the exec channel).
	SFTPEnabled *bool `json:"sftp_enabled,omitempty"`

	// SSHForwardSockets extends the default unix-socket allowlist that
	// gates `direct-streamlocal@openssh.com` channel-opens — the primitive
	// behind `podman --connection=<host>` (and any other SSH client that
	// asks to forward to a remote unix socket, including `ssh -L
	// localport:/remote.sock`). Defaults to the podman/docker sockets
	// outpost can discover automatically (see DetectPodman + the canonical
	// docker socket paths in ssh.go). Add absolute paths here to allow
	// additional sockets; entries are exact-matched after filepath.Clean
	// (no globbing). When SSHAllowLocalForward is off, this list is
	// ignored — the master switch wins.
	SSHForwardSockets []string `json:"ssh_forward_sockets,omitempty"`

	// Built-in proxies for local daemons. Default off (plain bool) — these
	// expose external infrastructure rather than outpost-owned routes, so
	// they require explicit opt-in via the admin UI. The UI greys these
	// toggles out when the daemon isn't actually running on this host.
	PodmanEnabled bool `json:"podman_enabled,omitempty"`
	OllamaEnabled bool `json:"ollama_enabled,omitempty"`

	// OllamaPoolEnabled gates whether this outpost participates in
	// cloudbox's virtual LLM pool — the watcher pushes the local
	// /api/tags inventory to cloudbox and the /app/ollama/_pool/capacity
	// endpoint is mounted. Distinct from OllamaEnabled: the user can
	// expose their local Ollama as a per-host app (a private endpoint
	// only they reach) without contributing it to the shared multi-host
	// pool friends and other paired hosts can route to. Pointer-bool
	// with OllamaPoolOn() helper so existing configs (written before
	// pooling shipped) default to on whenever OllamaEnabled is on —
	// the most useful behavior for the typical operator.
	OllamaPoolEnabled *bool `json:"ollama_pool_enabled,omitempty"`

	// OtelEnabled gates the observability built-in apps that proxy
	// ycode's embedded Prometheus / Alertmanager / VictoriaLogs /
	// Jaeger / Perses stack through the matrix tunnel. Off by default
	// — this is a substantial surface increase (each ycode sub-path
	// becomes reachable through /h/<host>/app/otel-*) and it only
	// makes sense when ycode-serve is running.
	OtelEnabled bool `json:"otel_enabled,omitempty"`

	// OtelPoolEnabled controls whether cloudbox is allowed to federate
	// queries across this outpost. Distinct from OtelEnabled the same
	// way OllamaPoolEnabled is from OllamaEnabled: an operator can
	// expose the surfaces privately (per-host access only) without
	// contributing to the fleet-wide dashboard. Pointer-bool with
	// OtelPoolOn() so existing configs default to on whenever
	// OtelEnabled is on.
	OtelPoolEnabled *bool `json:"otel_pool_enabled,omitempty"`

	// YcodeEnabled gates outpost's ycode-aware features (the
	// YcodeShare surfaces below, the OTel proxy wiring in main.go).
	// Detection-only — outpost never spawns or restarts `ycode
	// serve`. The operator manages ycode's lifecycle directly so
	// the flags they launched it with stay intact; outpost just
	// reads the manifest and reports state.
	//
	// ycode is the under-the-hood agentic engine outpost delegates
	// to for inference / podman / Gitea; one `ycode serve` per OS
	// user account. Distributed as a separate binary; the admin UI
	// surfaces a download link when no binary is found, mirroring
	// ycode's own TUI install flow.
	YcodeEnabled bool `json:"ycode_enabled,omitempty"`

	// YcodeShareEnabled gates whether ycode's home/landing page (the
	// SPA served at /ycode/ on ycode's bearer-authed proxy) is exposed
	// through the matrix tunnel as a regular `ycode` built-in app.
	// When on, cloudbox renders a tile and users can open the ycode UI
	// from the portal; when off, ycode stays purely-local and unreachable
	// through cloudbox. Pointer-bool with YcodeShareOn() helper so the
	// default is on whenever ycode itself is on — the typical operator
	// who turned ycode on probably wants to reach it remotely too.
	YcodeShareEnabled *bool `json:"ycode_share_enabled,omitempty"`

	// YcodeShareRequireLogin controls the cloudbox-side OS-password
	// elevation gate for the `ycode` built-in app. Default false
	// (matches custom-app conventions): owners of the host reach their
	// own ycode in one click. Flip to true to require an OS-password
	// elevation hop the way /shell and /desktop do — useful when the
	// host is shared with non-trivially-trusted users. Pointer-bool so
	// the absent-key case folds to the safer default ("no extra dance
	// for owners"); explicit true is honored.
	YcodeShareRequireLogin *bool `json:"ycode_share_require_login,omitempty"`

	// YcodeShareSurfaces is the per-surface opt-in overlay for the
	// ycode-share catalog (see internal/agent/otel/ycode_surfaces.go).
	// Map keys are tile names (`ycode`, `ycode-canvas`, `ycode-ollama`,
	// `ycode-git`, `ycode-memos`, `ycode-graph`); values are explicit
	// on/off. Absent keys fall back to the catalog's DefaultOn — today
	// only `ycode` (the chat) is default-on, so an operator who just
	// flips ycode_share_enabled gets the chat tile and nothing else
	// until they opt in to additional surfaces from the SPA.
	YcodeShareSurfaces map[string]bool `json:"ycode_share_surfaces,omitempty"`

	// UpdateMode is the per-host policy for cloudbox-pushed
	// self-upgrades at POST /admin/upgrade. Three values:
	//
	//   - "auto"   — default. Stage + probe + swap + restart on push.
	//   - "manual" — daemon persists the envelope to
	//                <cacheDir>/outpost/upgrade.pending.json and
	//                returns 202 pending_manual. Operator applies via
	//                `outpost upgrade apply` or cloudbox's UI button
	//                (which re-POSTs with Force=true to bypass the
	//                manual gate).
	//   - "never"  — refuse all cloudbox pushes; daemon returns 403.
	//
	// Empty / missing → "auto" (the default for paired hosts; making
	// it opt-in per host would defeat the "press button, fleet rolls"
	// promise). Use UpdateModeName() to read — it folds the empty
	// case and normalizes legacy AutoUpgrade *bool configs.
	UpdateMode string `json:"update_mode,omitempty"`

	// AutoUpgrade is the legacy boolean. Kept on the struct so old
	// agent.json files round-trip without losing data; LoadFile maps
	// it into UpdateMode on read (true → auto, false → never) and
	// writes clear UpdateMode going forward. New code reads via
	// UpdateModeName() — do not consult this field directly.
	AutoUpgrade *bool `json:"auto_upgrade,omitempty"`

	// AdminSessionKey is the HMAC secret used to sign admin-UI session
	// cookies. Persisting it across restarts is what keeps the admin user
	// logged in when a built-in toggle re-execs the binary. Base64-encoded
	// in the JSON (32 random bytes worth of entropy). Auto-generated and
	// saved on first boot via EnsureAdminSessionKey.
	AdminSessionKey []byte `json:"admin_session_key,omitempty"`

	// MCPBearerToken is the shared secret agent tools (Claude Code,
	// Windsurf, the outpost CLI, ...) present in Authorization: Bearer
	// headers when calling the MCP server mounted at /mcp/* on the same
	// loopback listener as the admin UI. Distinct from the session
	// cookie used by humans hitting /api/*. 32 random bytes encoded as
	// hex (64 chars) so it can be pasted into a .mcp.json verbatim.
	// Auto-generated on first boot via EnsureMCPBearerToken; the admin
	// UI exposes a "rotate" action that re-mints it.
	MCPBearerToken string `json:"mcp_bearer_token,omitempty"`

	// Outbound configures local mount paths that proxy through cloudbox to
	// remote outposts' apps. The local outpost holds an in-memory
	// elevation cookie per entry (captured by Connect); after that, the
	// local URL http://localhost:17777/<path>/ proxies to
	// https://<cloudbox>/h/<host>/app/<name>/<rest>. See
	// internal/agent/outbound.go.
	Outbound []OutboundConfig `json:"outbound,omitempty"`

	// Cluster, when present and Enabled, opts this outpost into the
	// cloudbox virtual-podman cluster: vkpodman joins a cloud-side k3s
	// API server as a virtual node and runs scheduled Pods as local
	// podman containers. See internal/agent/vkpodman. Off by default.
	Cluster *ClusterConfig `json:"cluster,omitempty"`

	// LAN peer discovery + LAN-direct dial (Wave 3A). All default off
	// so a default install doesn't leak metadata or expose listeners.

	// AssignedHostname is the cloudbox-issued DNS-safe slug returned at
	// register/exchange time (e.g. "dragon-7a3b"). Used as the mDNS
	// service-instance name and as the assumed hostname for
	// `<assigned_hostname>.local` resolution. Cloudbox-side issuance
	// lands in Wave 3A.2; until then this falls back to os.Hostname()
	// in the daemon's startup path.
	AssignedHostname string `json:"assigned_hostname,omitempty"`

	// OAuth2Email is the cloudbox account-owner identity (the OAuth2
	// "I am the resource owner" claim). Returned by the
	// register/exchange flow in Wave 3A.2. Used as the Tier-2 trust
	// anchor by PeerTrustPolicy="same-owner".
	OAuth2Email string `json:"oauth2_email,omitempty"`

	// OSUsername is the OS user the outpost daemon runs as
	// (informational + future SSH user-cert flow). Populated from
	// hostauth.CurrentUser() at boot when empty.
	OSUsername string `json:"os_username,omitempty"`

	// DiscoveryEnabled gates mDNS advertisement and the HTTP /discover
	// surface mount. Default off — flip on with `outpost config set
	// --discovery=on` once the operator understands the privacy
	// posture (mDNS broadcasts hostname + fingerprint on the LAN).
	DiscoveryEnabled *bool `json:"discovery_enabled,omitempty"`

	// SSHListenAddr is the optional LAN TCP bind for the in-process SSH
	// server (e.g. "0.0.0.0:2222"). Empty disables the LAN listener;
	// the matrix tunnel /ssh endpoint stays the only path until the
	// operator explicitly opts into LAN exposure. The same handleSSHConn
	// services WS and LAN paths; PasswordCallback authentication
	// applies on LAN-direct (no cloudbox vouching available).
	SSHListenAddr string `json:"ssh_listen_addr,omitempty"`

	// DiscoveryHTTPListenAddr is the optional LAN bind for the HTTP
	// /api/v1/discover/* surface (e.g. "0.0.0.0:17778"). Empty disables.
	// When set, advertised in mDNS TXT and cloudbox peer-hints so
	// other outposts can probe us directly.
	DiscoveryHTTPListenAddr string `json:"discovery_http_listen_addr,omitempty"`

	// PeerTrustPolicy controls which discovered peers we'll accept for
	// Tier-2 operations (ssh exec, jump, sftp, repair). One of:
	//
	//   "same-owner"     — default; require oauth2_email match
	//   "same-cloudbox"  — accept any peer paired with our cloudbox
	//   "tofu-allow"     — fall back to TOFU on fingerprint when no cert
	//
	// The default refuses peers in the same cloudbox but a different
	// OAuth2 account — strangers shouldn't be able to jump-host or
	// install-upgrade through my outpost just because we share a
	// cloudbox tenant.
	PeerTrustPolicy string `json:"peer_trust_policy,omitempty"`
}

// ClusterConfig persists the kubeconfig fields cloudbox issues at
// "join cluster" time. APIURL/Token/CA together build a client-go
// rest.Config; NodeName defaults to AgentName.
//
// We store the three credential fields directly (rather than parsing a
// kubeconfig file on every boot) so the join flow can accept a pasted
// kubeconfig once, persist what matters, and be done. Token rotation
// becomes a one-line file save instead of a file-format dance.
type ClusterConfig struct {
	// Enabled is the master switch. When false, the rest is ignored and
	// neither the vkpodman loop nor the k3s-agent supervisor starts.
	Enabled bool `json:"enabled,omitempty"`

	// Mode selects which runtime joins the cluster on this outpost:
	//   - "" or "vkpodman" — legacy v1 virtual-kubelet that translates
	//     k8s Pods to local podman containers (per-outpost pod-shape
	//     limits: no PodIP, PVC, init/sidecar containers, etc.)
	//   - "agent" — real `k3s agent` subprocess that joins as a normal
	//     kubelet via the matrix-tunnel STCP visitor (Phase 1 of the
	//     "real shared k8s" plan; Linux-only).
	// Default empty for backward compat. Cloudbox does not push a Mode
	// at pairing time — operator sets this via `outpost builtins set
	// --cluster-mode=agent`.
	Mode string `json:"mode,omitempty"`

	// APIURL is the cluster's apiserver — typically the cloudbox-proxied
	// URL like https://ai.dhnt.io/api/cluster/agent for production, or
	// https://127.0.0.1:6443 against a local k3s for dev/PoC.
	APIURL string `json:"api_url,omitempty"`
	// Token is the bearer credential. For production this is a
	// per-host ServiceAccount token cloudbox issued; for dev/PoC it
	// can come straight out of /etc/rancher/k3s/k3s.yaml.
	Token string `json:"token,omitempty"`
	// CA is the apiserver's TLS CA bundle (PEM). Required when APIURL
	// is https://. Empty means "trust the system roots" — fine when
	// cloudbox fronts the apiserver behind a real cert.
	CA []byte `json:"ca,omitempty"`
	// NodeName is the name we register with. Defaults to AgentName when
	// empty (so `kubectl get nodes` shows the same hostname the portal
	// uses) but can be overridden if multiple outposts on the same host
	// want distinct cluster identities.
	NodeName string `json:"node_name,omitempty"`

	// NodeToken is the k3s join token (K10…::node:…) cloudbox handed
	// out at register time. Consumed only by Mode="agent"; passed as
	// `k3s agent --token`. Empty when cloudbox isn't running in cluster
	// mode or hasn't materialized the token yet (re-pair to refresh).
	NodeToken string `json:"node_token,omitempty"`

	// STCPSecret authenticates the local frp STCP visitor that opens a
	// 127.0.0.1:<K8sAPIPort> listener and tunnels each accepted conn to
	// cloudbox's embedded apiserver. Cluster-wide; minted by cloudbox at
	// register time. Consumed only by Mode="agent".
	STCPSecret string `json:"stcp_secret,omitempty"`

	// K8sAPIPort is the TCP port the STCP visitor binds locally for the
	// apiserver listener. `k3s agent --server` dials
	// https://127.0.0.1:<K8sAPIPort>. Matches cloudbox's
	// ClusterAPIServerPort so kubeconfigs round-trip cleanly. Default
	// 6443 when empty.
	K8sAPIPort int `json:"k8s_api_port,omitempty"`

	// KubeletProxyPort is the per-host loopback port ON CLOUDBOX where
	// the matrix tunnel exposes this outpost's kubelet (Phase 2). The
	// outpost's matrix-tunnel client registers a TCPProxy with
	// LocalPort=10250, RemotePort=KubeletProxyPort so cloudbox's
	// embedded apiserver can dial through 127.0.0.1:<KubeletProxyPort>
	// to reach `kubectl logs/exec` targets. Empty when cloudbox has
	// cluster mode off OR when the kubelet port pool was exhausted at
	// Exchange time — in which case the outpost just doesn't publish
	// the proxy (the rest of cluster-agent mode still works).
	KubeletProxyPort int `json:"kubelet_proxy_port,omitempty"`

	// OverlayLoginServer is the URL the outpost's tailscaled connects
	// to (--login-server) for coordination. In production this is
	// cloudbox's public URL + /overlay/headscale. Empty when the
	// cloudbox-side overlay is off — outpost then doesn't start
	// tailscaled and no overlay/CNI plumbing is set up. Phase 3.
	OverlayLoginServer string `json:"overlay_login_server,omitempty"`

	// OverlayAuthKey is the one-shot pre-auth key the outpost passes
	// as `tailscale up --authkey=<key>`. Minted by cloudbox at
	// Exchange time. Phase 3.
	OverlayAuthKey string `json:"overlay_auth_key,omitempty"`

	// OverlayPodCIDR is the /24 cloudbox allocated to this outpost
	// from CLUSTER_POD_CIDR. The outpost passes it as
	// `tailscale up --advertise-routes=<cidr>` so other outposts can
	// route to this node's pods, AND the (Phase 3b) CNI plugin uses
	// it as the per-pod IP pool. Phase 3.
	OverlayPodCIDR string `json:"overlay_pod_cidr,omitempty"`

	// MetricsRemoteURL / LogsRemoteURL / TracesRemoteURL are the
	// observability fleet-aggregation endpoints cloudbox has
	// provisioned in the cluster (typically backed by VictoriaMetrics /
	// VictoriaLogs / Jaeger Apache 2.0 stacks deployed via the
	// AppStore). When non-empty, ycode's collector is expected to
	// remote_write metrics / push logs / OTLP-export traces to these
	// URLs through the tailscale overlay — the symmetric "push" side
	// of the per-host /app/otel-* reverse-proxy surfaces, supplying
	// fleet-wide dashboards without cloudbox itself storing anything.
	//
	// Resolution path: cluster Service DNS reachable on the overlay
	// (e.g. http://vmsingle.observability.svc.cluster.local:8428/api/v1/write).
	// Empty values mean "no fleet aggregation configured" — the local
	// per-outpost stack is still queryable through the matrix tunnel.
	//
	// Persisted at register time from the Exchange response; cloudbox
	// is the source of truth. Outpost doesn't synthesize defaults
	// because the cluster service names depend on operator naming
	// choices at AppStore install time.
	MetricsRemoteURL string `json:"metrics_remote_url,omitempty"`
	LogsRemoteURL    string `json:"logs_remote_url,omitempty"`
	TracesRemoteURL  string `json:"traces_remote_url,omitempty"`
}

// ClusterModeAgent reports whether the outpost should run the real
// `k3s agent` path rather than the v1 vkpodman virtual-kubelet.
// Centralized so future modes can be added without touching every
// call site.
//
// Semantics:
//
//   - Mode == "agent" → agent
//   - Mode == "vkpodman" → vkpodman (explicit opt-out)
//   - Mode == "" → agent (the default; the real k3s-agent path is
//     canonical for 100% K8s compliance on every supported OS — agent
//     mode runs k3s-agent inside the `outpost-runtime` container via
//     podman, which works on Linux, macOS, and Windows hosts wherever
//     podman is available).
func (c *ClusterConfig) ClusterModeAgent() bool {
	if c == nil {
		return false
	}
	switch c.Mode {
	case "vkpodman":
		return false
	default: // "agent" or empty
		return true
	}
}

// OutboundConfig is one local mount that proxies to a remote outpost.
//
//   - Path : local mount identifier. For Scheme=="http" this is the
//     subpath under the admin UI listener — e.g. "kg" makes the remote
//     app reachable at http://localhost:17777/kg/. For Scheme=="tcp" it
//     is also the addressing key (used in the API URLs and for state
//     lookup) but no HTTP subpath is mounted.
//   - Name : the remote outpost's app name (e.g. "ollama", "postgres").
//     Matched against the remote's AppRegistry by the cloudbox host-proxy.
//   - Host : the remote outpost's name as registered with cloudbox.
//   - User : the OS user on the remote outpost (used at Connect time
//     when POSTing to /h/<host>/elevate).
//   - Scheme:
//   - "http" (default): local mount is the admin-UI subpath
//     http://localhost:17777/<Path>/... proxied through cloudbox to
//     the remote outpost's /app/<Name>/ http app.
//   - "tcp": local outpost opens a 127.0.0.1:LocalPort listener
//     after Connect and bridges every accepted TCP conn through
//     cloudbox as a WebSocket to the remote outpost's tcp-scheme
//     app named <Name>. Lets unmodified clients reach non-HTTP
//     services (ssh, psql, mysql) the remote outpost has registered
//     as TCP apps.
//   - "ssh": same listener+WS-bridge shape as "tcp", but the bridge
//     targets the remote outpost's built-in /ssh endpoint (the
//     in-process Go SSH server) directly — no app registration on
//     the remote required. Name is ignored. Elevate flow uses
//     host-level /h/<Host>/elevate (the same one outpost ssh-proxy
//     /outpost connect uses), so the matrix_elev cookie scope is the
//     whole host rather than a single app.
//   - LocalPort: required for Scheme=="tcp" or "ssh". Ignored otherwise.
//   - TTLSeconds: per-mount override for cloudbox's absolute-expiry cap on
//     the matrix_elev cookie. 0 (unset) uses the cloudbox default;
//     math.MaxInt64 means "no absolute cap, only idle expiry" — useful
//     for long-running agentic sessions. Cloudbox must honor the
//     ttl_seconds field in the elevate POST body for this to take effect;
//     older cloudbox versions ignore it and apply their default.
type OutboundConfig struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	User       string `json:"user"`
	Scheme     string `json:"scheme,omitempty"`
	LocalPort  int    `json:"local_port,omitempty"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
}

// SchemeNorm returns the effective scheme — empty defaults to "http" so
// configs written before TCP support landed keep their old behavior.
// Recognized values: "http", "tcp", "ssh".
func (oc OutboundConfig) SchemeNorm() string {
	s := strings.ToLower(strings.TrimSpace(oc.Scheme))
	if s == "" {
		return "http"
	}
	return s
}

// BindsListener reports whether this outbound, when Connected, owns a
// 127.0.0.1:LocalPort TCP listener. True for "tcp" and "ssh" (both
// expose the remote service as a local port); false for "http" (which
// is served as a subpath under the admin-UI listener).
func (oc OutboundConfig) BindsListener() bool {
	s := oc.SchemeNorm()
	return s == "tcp" || s == "ssh"
}

// BuiltinSSH reports whether this outbound targets the remote outpost's
// built-in /ssh WebSocket endpoint (rather than a registered app under
// /app/<name>/). True only for Scheme=="ssh".
func (oc OutboundConfig) BuiltinSSH() bool {
	return oc.SchemeNorm() == "ssh"
}

// AppConfig is one custom reverse-proxy target. It is mounted under
// /app/<name>/ on the agent and the cloud reaches it through the tunnel.
//
// Scheme picks the transport:
//   - "http" / "https": classic TCP target. Use Host (default 127.0.0.1)
//     and Port. Socket is ignored.
//   - "unix": AF_UNIX socket at Socket. Works on Linux, macOS, and
//     Windows (AF_UNIX since Win10 1803). Host/Port are ignored.
//   - "npipe": Windows named pipe at Socket (e.g. \\.\pipe\docker_engine).
//     Only supported on Windows builds; non-Windows builds reject it at
//     request time. Host/Port are ignored.
//   - "tcp": raw TCP target at Host:Port. The agent does NOT speak HTTP
//     to such an app; instead the /app/<name>/ route accepts a
//     WebSocket upgrade and byte-bridges WS↔TCP. Reached from a remote
//     outpost via a tcp-scheme outbound (see OutboundConfig). Used for
//     ssh, postgres, mysql, redis and other non-HTTP services.
type AppConfig struct {
	Name    string `json:"name"`
	Icon    string `json:"icon,omitempty"`
	Scheme  string `json:"scheme"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Socket  string `json:"socket,omitempty"`
	Enabled bool   `json:"enabled"`

	// RequireLogin: when true, outpost serves /app/<name>/* only when
	// the inbound request carries cloudbox-vouched proof of local-OS
	// authentication (the X-Periscope-Role header cloudbox stamps
	// after a successful /elevate flow). Without it the request gets
	// 403. Default true; the opt-out is for genuinely public surfaces.
	// Replaces the legacy three-tier `role` field.
	RequireLogin bool `json:"require_login"`

	// LANOnlyPaths lists path prefixes (e.g. "/kiosk") that must NOT
	// be reachable through cloudbox. Outpost 404s when the inbound
	// request carries X-Forwarded-Prefix (= came via cloud) AND its
	// post-/app/<name>/ path matches one of these. Direct loopback /
	// LAN access (no cloudbox hop) keeps working — that's where
	// kiosk-style public-but-local endpoints belong.
	LANOnlyPaths []string `json:"lan_only_paths,omitempty"`

	// IndexPath is an optional landing-page sub-path the cloudbox SPA
	// prepends to this app's tile URL. Default empty (= "/"). Lets
	// two AppConfig rows point at the same host:port and present as
	// two tiles — e.g. one row "class" with IndexPath="" lands on
	// the home page, a second row "class-admin" with
	// IndexPath="/admin" lands on the admin page. The proxy itself
	// does NOT use IndexPath when forwarding — it just forwards
	// `rest` literally. The payoff is per-tier sharing: each
	// virtual app gets its own HostShare rows, its own Connect /
	// cookie scope, its own RequireLogin and LANOnlyPaths.
	IndexPath string `json:"index_path,omitempty"`

	// TrustCloudIdentity opts this app into the trusted-header SSO
	// contract: when set, outpost forwards the cloudbox-vouched caller
	// identity to the upstream as Remote-User / Remote-Email /
	// Remote-Groups (the Authelia / oauth2-proxy / nginx-auth-request
	// lingua franca) and also passes through X-Periscope-User /
	// X-Periscope-Role. Off by default so existing apps keep their own
	// login UI; flip on for apps configured to trust dhnt.io.
	//
	// Stamping is conditional on the request having come through the
	// matrix tunnel (X-Forwarded-Prefix present). Direct loopback /
	// LAN hits never carry stamped identity regardless of this flag —
	// see the always-on Remote-* / X-Periscope-* sanitization in
	// internal/agent/apps.go's Rewrite callback.
	TrustCloudIdentity bool `json:"trust_cloud_identity,omitempty"`

	// ProvisioningToken is the opaque bearer the app uses when
	// pushing user grants up to cloudbox via outpost's
	// /_periscope/apps/<name>/users relay. Auto-generated (32 bytes,
	// hex) when the admin UI flips TrustCloudIdentity on or the
	// operator rotates it. Empty means provisioning is not yet
	// enabled — the relay endpoint refuses the request. Stored in
	// agent.json (mode 0600) and redacted out of the admin UI's
	// safeView (presence reported separately).
	ProvisioningToken string `json:"provisioning_token,omitempty"`

	// Role is deprecated. Kept for back-compat parsing of older
	// agent.json files. NewFromJSON migrates "guest" → RequireLogin
	// false; "user"/"admin"/empty → true.
	Role string `json:"role,omitempty"`
}

// IsSocket reports whether ac targets a local socket (unix or npipe)
// rather than a TCP host:port.
func (ac AppConfig) IsSocket() bool {
	s := strings.ToLower(strings.TrimSpace(ac.Scheme))
	return s == "unix" || s == "npipe"
}

// IsTCP reports whether ac is a raw-TCP app (ssh/postgres/etc.) that
// the agent exposes via /app/<name>/ as a WebSocket-to-TCP bridge.
func (ac AppConfig) IsTCP() bool {
	return strings.EqualFold(strings.TrimSpace(ac.Scheme), "tcp")
}

// AppTargetFromURL parses a single URL string ("http://localhost:8080",
// "unix:///run/podman/podman.sock", etc.) into the scheme/host/port/
// socket fields that AppConfig stores. The admin UI sends URLs; the
// server splits them here so the persisted record stays in the same
// shape that older configs and the AppRegistry already understand.
//
// http/https URLs use the default port when none is given (80/443).
// unix URLs may use either a `unix:///abs/path` or `unix:/abs/path`
// form; both are accepted. Returns an error on anything else.
func AppTargetFromURL(raw string) (scheme, host string, port int, socket string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0, "", fmt.Errorf("url is required")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", 0, "", fmt.Errorf("parse url: %w", perr)
	}
	scheme = strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
		host = u.Hostname()
		if host == "" {
			return "", "", 0, "", fmt.Errorf("url %q is missing host", raw)
		}
		if p := u.Port(); p != "" {
			n, cerr := strconv.Atoi(p)
			if cerr != nil || n < 1 || n > 65535 {
				return "", "", 0, "", fmt.Errorf("url %q has invalid port", raw)
			}
			port = n
		} else if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
		return scheme, host, port, "", nil
	case "tcp":
		host = u.Hostname()
		if host == "" {
			return "", "", 0, "", fmt.Errorf("url %q is missing host", raw)
		}
		p := u.Port()
		if p == "" {
			return "", "", 0, "", fmt.Errorf("url %q is missing port (required for tcp)", raw)
		}
		n, cerr := strconv.Atoi(p)
		if cerr != nil || n < 1 || n > 65535 {
			return "", "", 0, "", fmt.Errorf("url %q has invalid port", raw)
		}
		return scheme, host, n, "", nil
	case "unix", "npipe":
		// `unix:///path` → u.Path = "/path"; `unix:/path` → also "/path".
		// `unix://host/path` is technically valid but we treat the host
		// segment as advisory and use the path.
		sock := u.Path
		if sock == "" {
			sock = u.Opaque
		}
		if sock == "" {
			return "", "", 0, "", fmt.Errorf("url %q is missing socket path", raw)
		}
		return scheme, "", 0, sock, nil
	default:
		return "", "", 0, "", fmt.Errorf("url %q: scheme must be one of http|https|tcp|unix|npipe", raw)
	}
}

// ValidRole reports whether s is a recognized clearance level.
func ValidRole(s string) bool {
	switch s {
	case "", "guest", "user", "admin":
		return true
	}
	return false
}

// ShellOn reports whether the built-in /shell route should be mounted.
// Missing field (old configs) defaults to true.
func (fc *FileConfig) ShellOn() bool { return fc == nil || fc.ShellEnabled == nil || *fc.ShellEnabled }

// DesktopOn reports whether the built-in /desktop route should be mounted.
func (fc *FileConfig) DesktopOn() bool {
	return fc == nil || fc.DesktopEnabled == nil || *fc.DesktopEnabled
}

// ClipboardOn reports whether the built-in /clipboard route should be mounted.
func (fc *FileConfig) ClipboardOn() bool {
	return fc == nil || fc.ClipboardEnabled == nil || *fc.ClipboardEnabled
}

// SSHOn reports whether the built-in /ssh route (real SSH server reached
// over WebSocket through the matrix tunnel) should be mounted.
func (fc *FileConfig) SSHOn() bool { return fc == nil || fc.SSHEnabled == nil || *fc.SSHEnabled }

// SSHAllowLocalForwardOn reports whether the SSH server should honor
// `direct-tcpip` channel-open requests (stock `ssh -L` / `ssh -D`).
// Missing field (old configs) defaults to true — the channel is still
// gated by a loopback-only destination allowlist regardless.
// SFTPOn reports whether the embedded SSH server should accept the "sftp"
// subsystem. Default-on for the same reason scp-just-works matters.
func (fc *FileConfig) SFTPOn() bool {
	return fc == nil || fc.SFTPEnabled == nil || *fc.SFTPEnabled
}

func (fc *FileConfig) SSHAllowLocalForwardOn() bool {
	return fc == nil || fc.SSHAllowLocalForward == nil || *fc.SSHAllowLocalForward
}

// SSHAllowRemoteForwardOn reports whether the SSH server should honor
// `tcpip-forward` global requests (stock `ssh -R`). Missing field (old
// configs) defaults to true — the bind address is still locked to
// loopback by the agent regardless.
func (fc *FileConfig) SSHAllowRemoteForwardOn() bool {
	return fc == nil || fc.SSHAllowRemoteForward == nil || *fc.SSHAllowRemoteForward
}

// SSHAllowAgentForwardOn reports whether the SSH server should accept
// `auth-agent-req@openssh.com` channel-request (stock `ssh -A`).
// Missing field (old configs) defaults to true — the per-session
// socket is created in a private tempdir with 0600 perms.
func (fc *FileConfig) SSHAllowAgentForwardOn() bool {
	return fc == nil || fc.SSHAllowAgentForward == nil || *fc.SSHAllowAgentForward
}

// PodmanOn reports whether the built-in podman proxy is enabled in this
// config. Unlike the loopback-only builtins above, podman is off by
// default — the admin UI flips it on after the daemon is detected.
func (fc *FileConfig) PodmanOn() bool { return fc != nil && fc.PodmanEnabled }

// OllamaOn reports whether the built-in Ollama proxy is enabled.
func (fc *FileConfig) OllamaOn() bool { return fc != nil && fc.OllamaEnabled }

// YcodeOn reports whether outpost's ycode-aware features (share
// surfaces, OTel wiring) are enabled. Detection-only — outpost
// never spawns or restarts `ycode serve`. Plain bool, no implicit
// default. See YcodeEnabled in the struct doc.
func (fc *FileConfig) YcodeOn() bool { return fc != nil && fc.YcodeEnabled }

// YcodeShareOn reports whether ycode's home/landing page should be
// exposed through the matrix tunnel as a `ycode` built-in app. Returns
// false when ycode itself is off (the share is a strict extension of
// the per-host proxy). When YcodeShareEnabled is nil, the default is
// to follow YcodeOn — operators who turned ycode on probably want it
// reachable. Explicit false (operator turned it off) is honored.
func (fc *FileConfig) YcodeShareOn() bool {
	if !fc.YcodeOn() {
		return false
	}
	if fc.YcodeShareEnabled == nil {
		return true
	}
	return *fc.YcodeShareEnabled
}

// YcodeShareRequireLoginOn reports whether the cloudbox-side OS-password
// elevation gate should fire for the `ycode` built-in app. Default false
// — owners reach their own ycode without the OS-password popup; flipping
// to true makes cloudbox treat ycode like /shell or /desktop.
func (fc *FileConfig) YcodeShareRequireLoginOn() bool {
	if fc == nil || fc.YcodeShareRequireLogin == nil {
		return false
	}
	return *fc.YcodeShareRequireLogin
}

// ClusterOn reports whether this outpost should join the cloudbox
// virtual-podman cluster on boot. Missing field or Enabled=false ⇒ false.
func (fc *FileConfig) ClusterOn() bool {
	return fc != nil && fc.Cluster != nil && fc.Cluster.Enabled
}

// DiscoveryOn reports whether LAN discovery (mDNS + HTTP /discover)
// should be active. Default off; the *bool gives us an explicit
// opt-in semantic that survives the absent-key case.
func (fc *FileConfig) DiscoveryOn() bool {
	return fc != nil && fc.DiscoveryEnabled != nil && *fc.DiscoveryEnabled
}

// EffectivePeerTrustPolicy returns the configured policy with a
// default of "same-owner" when unset or invalid. Centralized so
// every consumer reaches the same fallback.
func (fc *FileConfig) EffectivePeerTrustPolicy() string {
	if fc == nil {
		return "same-owner"
	}
	switch strings.TrimSpace(fc.PeerTrustPolicy) {
	case "same-owner", "same-cloudbox", "tofu-allow":
		return strings.TrimSpace(fc.PeerTrustPolicy)
	}
	return "same-owner"
}

// EffectiveAssignedHostname returns AssignedHostname when set,
// otherwise a DNS-safe form of AgentName, otherwise os.Hostname().
// The Wave 3A.1 daemon uses this until cloudbox-side issuance lands
// in 3A.2.
func (fc *FileConfig) EffectiveAssignedHostname() string {
	if fc != nil {
		if h := strings.TrimSpace(fc.AssignedHostname); h != "" {
			return sanitizeHostname(h)
		}
		if h := strings.TrimSpace(fc.AgentName); h != "" {
			return sanitizeHostname(h)
		}
	}
	hn, _ := osHostname()
	return sanitizeHostname(hn)
}

// sanitizeHostname produces a DNS-safe label: lowercase letters,
// digits, hyphens. Anything else collapses to "-". Truncated to 63
// chars (DNS label limit).
func sanitizeHostname(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, byte(r))
		case r >= '0' && r <= '9':
			out = append(out, byte(r))
		case r == '-' || r == '_':
			out = append(out, '-')
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	// Trim trailing hyphens, cap at 63 chars.
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	if len(out) > 63 {
		out = out[:63]
	}
	if len(out) == 0 {
		return "outpost"
	}
	return string(out)
}

// osHostname is a tiny indirection so tests can stub it. Falls back
// to "outpost" when the OS call fails (rare).
var osHostname = os.Hostname

// ClusterNodeName returns the node identity to register with — the
// explicit override when set, otherwise AgentName.
func (fc *FileConfig) ClusterNodeName() string {
	if fc == nil || fc.Cluster == nil {
		return ""
	}
	if n := strings.TrimSpace(fc.Cluster.NodeName); n != "" {
		return n
	}
	return fc.AgentName
}

// UpdateModeAuto / UpdateModeManual / UpdateModeNever are the legal
// values of FileConfig.UpdateMode. Kept as package constants so the
// validation layers (admincore, MCP arg parsing) share one source of
// truth.
const (
	UpdateModeAuto   = "auto"
	UpdateModeManual = "manual"
	UpdateModeNever  = "never"
)

// UpdateModeName returns the normalized update-mode for this config.
// Folds the legacy AutoUpgrade *bool (true → auto, false → never)
// and defaults empty to "auto". Always one of the UpdateMode*
// constants; never returns "".
func (fc *FileConfig) UpdateModeName() string {
	if fc == nil {
		return UpdateModeAuto
	}
	switch fc.UpdateMode {
	case UpdateModeAuto, UpdateModeManual, UpdateModeNever:
		return fc.UpdateMode
	}
	// Empty / unknown — fold the legacy bool first.
	if fc.AutoUpgrade != nil {
		if *fc.AutoUpgrade {
			return UpdateModeAuto
		}
		return UpdateModeNever
	}
	return UpdateModeAuto
}

// ValidUpdateMode reports whether s is a legal value for UpdateMode.
// Mutators (admincore.SetBuiltins, MCP tool args) use this to reject
// bad inputs at the boundary.
func ValidUpdateMode(s string) bool {
	switch s {
	case UpdateModeAuto, UpdateModeManual, UpdateModeNever:
		return true
	}
	return false
}

// OllamaPoolOn reports whether this outpost should join cloudbox's LLM
// pool. Returns false when Ollama itself is off (the pool is a strict
// extension of the per-host proxy). When OllamaPoolEnabled is nil, the
// default is to follow OllamaOn — pooling is the useful behavior, and
// configs written before pooling shipped should opt in automatically.
// Explicit false (operator turned it off) is honored.
func (fc *FileConfig) OllamaPoolOn() bool {
	if !fc.OllamaOn() {
		return false
	}
	if fc.OllamaPoolEnabled == nil {
		return true
	}
	return *fc.OllamaPoolEnabled
}

// OtelOn reports whether the built-in observability proxies are enabled.
func (fc *FileConfig) OtelOn() bool { return fc != nil && fc.OtelEnabled }

// OtelPoolOn reports whether this outpost participates in cloudbox's
// federated dashboard / alert fan-out. Mirrors OllamaPoolOn: false when
// OtelOn() is false; defaults to on when OtelPoolEnabled is nil.
func (fc *FileConfig) OtelPoolOn() bool {
	if !fc.OtelOn() {
		return false
	}
	if fc.OtelPoolEnabled == nil {
		return true
	}
	return *fc.OtelPoolEnabled
}

// SaveFile writes fc atomically (write+rename) to path, creating parents.
func SaveFile(path string, fc *FileConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(fc); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EnsureAdminSessionKey returns fc.AdminSessionKey, generating a fresh
// 32-byte random key (and persisting it via SaveFile at path) if the
// field is empty. Callers MUST pass a non-nil fc that they've already
// loaded (or freshly constructed). The returned slice points at the
// same backing array as fc.AdminSessionKey.
//
// Why this lives here: the key has to outlive the process, so it
// belongs in the on-disk FileConfig; but it's the admin UI server that
// uses it. Centralizing the load-or-create here lets main.go thread it
// into adminui.Deps without duplicating the IO dance.
func EnsureAdminSessionKey(path string, fc *FileConfig) ([]byte, error) {
	if fc == nil {
		return nil, fmt.Errorf("nil FileConfig")
	}
	if len(fc.AdminSessionKey) >= 16 {
		return fc.AdminSessionKey, nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("generate admin session key: %w", err)
	}
	fc.AdminSessionKey = b[:]
	if path != "" {
		if err := SaveFile(path, fc); err != nil {
			return nil, fmt.Errorf("save admin session key: %w", err)
		}
	}
	return fc.AdminSessionKey, nil
}

// EnsureMCPBearerToken returns fc.MCPBearerToken, generating a fresh
// 32-byte random hex string (and persisting it via SaveFile at path)
// if the field is empty. Same shape as EnsureAdminSessionKey; the
// MCP token is hex (not raw bytes) so it can be pasted into a
// .mcp.json file verbatim.
func EnsureMCPBearerToken(path string, fc *FileConfig) (string, error) {
	if fc == nil {
		return "", fmt.Errorf("nil FileConfig")
	}
	if len(fc.MCPBearerToken) >= 32 {
		return fc.MCPBearerToken, nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate mcp bearer token: %w", err)
	}
	fc.MCPBearerToken = hex.EncodeToString(b[:])
	if path != "" {
		if err := SaveFile(path, fc); err != nil {
			return "", fmt.Errorf("save mcp bearer token: %w", err)
		}
	}
	return fc.MCPBearerToken, nil
}

// RotateMCPBearerToken forces a fresh token regardless of the current
// value, persists it, and returns the new value. The old token stops
// authenticating immediately. Callers (admin UI / CLI / MCP itself)
// must surface the new value so the operator can update their
// .mcp.json before the next call.
func RotateMCPBearerToken(path string, fc *FileConfig) (string, error) {
	if fc == nil {
		return "", fmt.Errorf("nil FileConfig")
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate mcp bearer token: %w", err)
	}
	fc.MCPBearerToken = hex.EncodeToString(b[:])
	if path != "" {
		if err := SaveFile(path, fc); err != nil {
			return "", fmt.Errorf("save mcp bearer token: %w", err)
		}
	}
	return fc.MCPBearerToken, nil
}

// LoadFile reads a previously-saved FileConfig. Returns (nil, nil) if the
// file doesn't exist — callers should fall back to env.
func LoadFile(path string) (*FileConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var fc FileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	migrateLegacyRole(&fc)
	return &fc, nil
}

// migrateLegacyRole folds the deprecated AppConfig.Role into the new
// RequireLogin boolean. Legacy mapping: "guest" → false; "user"/"admin"/
// "" → true. Once each app has been re-saved through the admin UI the
// Role field disappears and this function becomes a no-op.
func migrateLegacyRole(fc *FileConfig) {
	if fc == nil {
		return
	}
	for i := range fc.Apps {
		legacy := strings.ToLower(strings.TrimSpace(fc.Apps[i].Role))
		if legacy == "" {
			continue
		}
		// Only set RequireLogin when the JSON didn't explicitly set
		// it. Since the field is a non-pointer bool, "didn't set" is
		// indistinguishable from false — but here we're being
		// permissive: if Role says "user"/"admin", upgrade the bool.
		// Operators who genuinely want a public app (RequireLogin=
		// false) should drop the Role field at the same time.
		if legacy == "guest" {
			fc.Apps[i].RequireLogin = false
		} else {
			fc.Apps[i].RequireLogin = true
		}
		fc.Apps[i].Role = "" // drop the legacy field
	}
}
