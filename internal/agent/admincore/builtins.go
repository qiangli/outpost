package admincore

import (
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// BuiltinsParams is the partial-update shape for SetBuiltins. Pointer-
// bool fields mean "leave unchanged when nil"; non-nil fields are
// written through to the FileConfig.
//
// The set of fields here is broader than the admin SPA currently
// surfaces — SSHAllowRemoteForward, SSHAllowAgentForward, and
// SSHForwardSockets exist in FileConfig but the SPA has no toggle for
// them. MCP / CLI callers can drive them directly.
type BuiltinsParams struct {
	Shell                 *bool    `json:"shell,omitempty"`
	Desktop               *bool    `json:"desktop,omitempty"`
	Clipboard             *bool    `json:"clipboard,omitempty"`
	SSH                   *bool    `json:"ssh,omitempty"`
	SSHAllowLocalForward  *bool    `json:"ssh_allow_local_forward,omitempty"`
	SSHAllowRemoteForward *bool    `json:"ssh_allow_remote_forward,omitempty"`
	SSHAllowAgentForward  *bool    `json:"ssh_allow_agent_forward,omitempty"`
	SSHForwardSockets     []string `json:"ssh_forward_sockets,omitempty"`
	SFTP                  *bool    `json:"sftp,omitempty"`
	// Files builtin (embedded File Browser). Files toggles the mount;
	// FilesAllowWrite flips read-only⇄read-write (all write ops together);
	// FilesScope sets the confined root (nil = leave unchanged, empty
	// string = the OS user's home). FilesAllowWrite is intentionally only
	// settable here on the loopback admin plane — the cloud-facing surface
	// has no path to it, which is what keeps "read-only by default" a real
	// guarantee rather than a default.
	Files           *bool   `json:"files,omitempty"`
	FilesAllowWrite *bool   `json:"files_allow_write,omitempty"`
	FilesScope      *string `json:"files_scope,omitempty"`
	Podman          *bool   `json:"podman,omitempty"`
	Sandbox         *bool   `json:"sandbox,omitempty"`
	Ollama          *bool   `json:"ollama,omitempty"`
	OllamaPool      *bool   `json:"ollama_pool,omitempty"`
	// WarmServing toggles the adaptive, considerate warm-serving plane:
	// keep a small conservative set of models resident (zero cold-start),
	// yielding (unloading) whenever the host is busy with the user's own
	// work and restoring when idle. Default ON for a paired Ollama node.
	// WarmBudgetFrac sets the fraction of usable memory dedicated to warm
	// preload (clamped to (0,1]; default 0.33). nil = leave unchanged.
	WarmServing            *bool           `json:"warm_serving,omitempty"`
	WarmBudgetFrac         *float64        `json:"warm_budget_frac,omitempty"`
	Otel                   *bool           `json:"otel,omitempty"`
	OtelPool               *bool           `json:"otel_pool,omitempty"`
	Ycode                  *bool           `json:"ycode,omitempty"`
	YcodeShare             *bool           `json:"ycode_share,omitempty"`
	YcodeShareRequireLogin *bool           `json:"ycode_share_require_login,omitempty"`
	YcodeShareSurfaces     map[string]bool `json:"ycode_share_surfaces,omitempty"`
	Cluster                *bool           `json:"cluster,omitempty"`
	// ClusterMode selects which runtime joins the cluster:
	// "" / "vkpodman" / "vk-podman" → libpod virtual-kubelet (default).
	// "vk-native" → generic native-process virtual-kubelet backend.
	// "vk-ollama" → legacy native-process (ollama) virtual-kubelet backend.
	// "agent" → real `k3s agent` subprocess (Linux only). Pointer-string
	// so nil = "leave unchanged"; non-nil with an unknown value is
	// rejected by SetBuiltins with a 400-class APIError. The persisted
	// value is normalized via conf.NormalizeClusterMode.
	ClusterMode *string `json:"cluster_mode,omitempty"`
	// UpdateMode is one of "auto" / "manual" / "never" (see
	// conf.UpdateMode* constants). Pointer-string so nil = "leave
	// unchanged"; non-nil with an invalid value is rejected by
	// SetBuiltins with a 400-class APIError.
	UpdateMode *string `json:"update_mode,omitempty"`
	// AutoRollback arms the auto-rollback watchdog's DESTRUCTIVE revert
	// (default off / observe-only). nil = leave unchanged.
	AutoRollback *bool `json:"auto_rollback,omitempty"`
	// Mesh toggles the libp2p mesh data plane (the peer node carrying
	// authenticated, NAT-traversing peer↔peer streams). MeshPort sets its
	// TCP+QUIC listen port (0 = ephemeral). nil = leave unchanged.
	Mesh     *bool `json:"mesh,omitempty"`
	MeshPort *int  `json:"mesh_port,omitempty"`
	// LANInference toggles the same-LAN direct-inference listener: a
	// LAN-reachable reverse proxy to the local inference server, advertised
	// to cloudbox so same-LAN callers reach this host's LLM directly (lower
	// latency, bypassing the relay). LANInferencePort sets its listen port
	// (0 = default 11435). This is a LAN-TRUST endpoint (no per-request
	// auth) — an explicit opt-in. nil = leave unchanged.
	LANInference     *bool `json:"lan_inference,omitempty"`
	LANInferencePort *int  `json:"lan_inference_port,omitempty"`
	// Shard is the Ollama sharding sub-feature: serve a model bigger than one
	// node by splitting it across mesh peers. nil = leave unchanged; *bool=false
	// opts OUT of the zero-config default (on for an owner-registered Ollama
	// node). ShardPeers selects worker hostnames (nil = leave; empty/["auto"] =
	// every same-LAN peer); ShardRole is "auto"/"leader"/"worker".
	Shard      *bool    `json:"shard,omitempty"`
	ShardPeers []string `json:"shard_peers,omitempty"`
	ShardRole  *string  `json:"shard_role,omitempty"`
	// Back-compat convenience for the generic bashy service "loom".
	Loom     *bool `json:"loom,omitempty"`
	LoomPort *int  `json:"loom_port,omitempty"`
	// BashyServices replaces the whole generic service set when non-nil.
	BashyServices []conf.BashyService `json:"bashy_services,omitempty"`
	// BashyVersion pins the bashy release the self-heal auto-install fetches
	// when bashy is missing ("" / "latest" = newest; a tag pins it). Takes
	// effect on the next restart. nil = leave unchanged.
	BashyVersion *string `json:"bashy_version,omitempty"`
	// Zot toggles running the Zot OCI registry as a managed external binary on a
	// loopback port, auto-exposed over the mesh as `registry`. ZotPort sets its
	// HTTP port (0 = default 5000). nil = leave unchanged.
	Zot     *bool `json:"zot,omitempty"`
	ZotPort *int  `json:"zot_port,omitempty"`
	// Seaweedfs toggles running SeaweedFS (object/blob store, S3 gateway) as a
	// managed external binary on a loopback port, auto-exposed over the mesh as
	// `s3`. SeaweedfsPort sets its S3 port (0 = default 8333). nil = unchanged.
	Seaweedfs     *bool `json:"seaweedfs,omitempty"`
	SeaweedfsPort *int  `json:"seaweedfs_port,omitempty"`
	// Kopia toggles running the Kopia snapshot-backup repository server as a
	// managed external binary on a loopback port, auto-exposed over the mesh as
	// `backup`. KopiaPort sets its port (0 = default 51515). nil = unchanged.
	Kopia     *bool `json:"kopia,omitempty"`
	KopiaPort *int  `json:"kopia_port,omitempty"`

	// Actrunner toggles running Gitea act_runner (the CI executor) as a managed
	// external binary. Unlike loom/zot it's a CONSUMER: it registers against a
	// Gitea instance and dials OUT. ActrunnerInstance is the Gitea base URL
	// (empty = local loom forge); ActrunnerToken is the registration token;
	// ActrunnerLabels are the executor labels (default "host:host"). nil = unchanged.
	Actrunner         *bool   `json:"actrunner,omitempty"`
	ActrunnerInstance *string `json:"actrunner_instance,omitempty"`
	ActrunnerToken    *string `json:"actrunner_token,omitempty"`
	ActrunnerLabels   *string `json:"actrunner_labels,omitempty"`
	// CloudDOEnabled toggles Digital Ocean provider support; CloudDOToken is the
	// DO API token (exported as DIGITALOCEAN_ACCESS_TOKEN). nil = unchanged.
	CloudDOEnabled *bool   `json:"cloud_do_enabled,omitempty"`
	CloudDOToken   *string `json:"cloud_do_token,omitempty"`
	// ActrunnerSandbox opts the runner into the tier-3 sandbox (container)
	// executor (runs-on: sandbox → OCI container via bashy podman), additive to
	// the host build lane. ActrunnerSandboxImage / ActrunnerDockerHost override
	// the image / DOCKER_HOST (empty docker-host = auto-resolve bashy podman).
	ActrunnerSandbox      *bool   `json:"actrunner_sandbox,omitempty"`
	ActrunnerSandboxImage *string `json:"actrunner_sandbox_image,omitempty"`
	ActrunnerDockerHost   *string `json:"actrunner_docker_host,omitempty"`
}

// BuiltinsResult reports what happened. RestartPending is true when
// the change is one the tunnel / built-in routes need to reload to
// observe — callers should poll Status until the daemon is back.
type BuiltinsResult struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

// SetBuiltins applies the partial update p to the persisted FileConfig
// and (when the host is paired) schedules a restart so the new toggles
// take effect. On a first-time setup (AgentName empty) nothing is
// mounted yet, so the save is harmless and no restart is triggered.
func (s *Server) SetBuiltins(p BuiltinsParams) (BuiltinsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return BuiltinsResult{}, err
	}
	if p.Shell != nil {
		fc.ShellEnabled = p.Shell
	}
	if p.Desktop != nil {
		fc.DesktopEnabled = p.Desktop
	}
	if p.Clipboard != nil {
		fc.ClipboardEnabled = p.Clipboard
	}
	if p.SSH != nil {
		fc.SSHEnabled = p.SSH
	}
	if p.SSHAllowLocalForward != nil {
		fc.SSHAllowLocalForward = p.SSHAllowLocalForward
	}
	if p.SSHAllowRemoteForward != nil {
		fc.SSHAllowRemoteForward = p.SSHAllowRemoteForward
	}
	if p.SSHAllowAgentForward != nil {
		fc.SSHAllowAgentForward = p.SSHAllowAgentForward
	}
	if p.SSHForwardSockets != nil {
		fc.SSHForwardSockets = p.SSHForwardSockets
	}
	if p.SFTP != nil {
		fc.SFTPEnabled = p.SFTP
	}
	if p.Files != nil {
		fc.FilesEnabled = p.Files
	}
	if p.FilesAllowWrite != nil {
		fc.FilesAllowWrite = *p.FilesAllowWrite
	}
	if p.FilesScope != nil {
		fc.FilesScope = *p.FilesScope
	}
	if p.Podman != nil {
		fc.PodmanEnabled = *p.Podman
	}
	if p.Sandbox != nil {
		fc.SandboxEnabled = *p.Sandbox
	}
	if p.Ollama != nil {
		fc.OllamaEnabled = *p.Ollama
	}
	if p.OllamaPool != nil {
		fc.OllamaPoolEnabled = p.OllamaPool
	}
	if p.WarmServing != nil {
		fc.WarmServingEnabled = p.WarmServing
	}
	if p.WarmBudgetFrac != nil {
		if *p.WarmBudgetFrac <= 0 || *p.WarmBudgetFrac > 1 {
			return BuiltinsResult{}, badRequest("warm_budget_frac must be in the range (0, 1]")
		}
		fc.WarmBudgetFrac = *p.WarmBudgetFrac
	}
	if p.Mesh != nil {
		fc.MeshEnabled = p.Mesh
	}
	if p.MeshPort != nil {
		fc.MeshPort = *p.MeshPort
	}
	if p.LANInference != nil {
		fc.LANInferenceEnabled = p.LANInference
	}
	if p.LANInferencePort != nil {
		fc.LANInferencePort = *p.LANInferencePort
	}
	if p.Shard != nil {
		if fc.Shard == nil {
			fc.Shard = &conf.ShardConfig{}
		}
		fc.Shard.Enabled = p.Shard
	}
	if p.ShardPeers != nil {
		if fc.Shard == nil {
			fc.Shard = &conf.ShardConfig{}
		}
		fc.Shard.Peers = p.ShardPeers
	}
	if p.ShardRole != nil {
		if fc.Shard == nil {
			fc.Shard = &conf.ShardConfig{}
		}
		fc.Shard.Role = *p.ShardRole
	}
	if p.Loom != nil {
		fc.LoomEnabled = p.Loom
	}
	if p.LoomPort != nil {
		fc.LoomPort = *p.LoomPort
	}
	if p.Loom != nil || p.LoomPort != nil {
		upsertBashyService(&fc.BashyServices, conf.BashyService{
			Name:         "loom",
			Enabled:      p.Loom == nil || *p.Loom,
			AppName:      "loom",
			AppPort:      fc.LoomPortOrDefault(),
			RequireLogin: true,
			MeshService:  "git",
		}, p.Loom, p.LoomPort)
	}
	if p.BashyServices != nil {
		fc.BashyServices = normalizeBashyServices(p.BashyServices)
	}
	if p.Zot != nil {
		fc.ZotEnabled = p.Zot
	}
	if p.ZotPort != nil {
		fc.ZotPort = *p.ZotPort
	}
	if p.Seaweedfs != nil {
		fc.SeaweedfsEnabled = p.Seaweedfs
	}
	if p.SeaweedfsPort != nil {
		fc.SeaweedfsPort = *p.SeaweedfsPort
	}
	if p.Kopia != nil {
		fc.KopiaEnabled = p.Kopia
	}
	if p.KopiaPort != nil {
		fc.KopiaPort = *p.KopiaPort
	}
	if p.Actrunner != nil {
		fc.ActrunnerEnabled = p.Actrunner
	}
	if p.BashyVersion != nil {
		fc.BashyVersion = strings.TrimSpace(*p.BashyVersion)
	}
	if p.ActrunnerInstance != nil {
		fc.ActrunnerInstance = *p.ActrunnerInstance
	}
	if p.ActrunnerToken != nil {
		fc.ActrunnerToken = *p.ActrunnerToken
	}
	if p.CloudDOEnabled != nil {
		fc.CloudDOEnabled = p.CloudDOEnabled
	}
	if p.CloudDOToken != nil {
		fc.CloudDOToken = *p.CloudDOToken
	}
	if p.ActrunnerLabels != nil {
		fc.ActrunnerLabels = *p.ActrunnerLabels
	}
	if p.ActrunnerSandbox != nil {
		fc.ActrunnerSandbox = p.ActrunnerSandbox
	}
	if p.ActrunnerSandboxImage != nil {
		fc.ActrunnerSandboxImage = *p.ActrunnerSandboxImage
	}
	if p.ActrunnerDockerHost != nil {
		fc.ActrunnerDockerHost = *p.ActrunnerDockerHost
	}
	if p.Otel != nil {
		fc.OtelEnabled = *p.Otel
	}
	if p.OtelPool != nil {
		fc.OtelPoolEnabled = p.OtelPool
	}
	if p.Ycode != nil {
		fc.YcodeEnabled = *p.Ycode
	}
	if p.YcodeShare != nil {
		fc.YcodeShareEnabled = p.YcodeShare
	}
	if p.YcodeShareRequireLogin != nil {
		fc.YcodeShareRequireLogin = p.YcodeShareRequireLogin
	}
	if p.YcodeShareSurfaces != nil {
		// Merge: caller's partial map overrides existing keys; other
		// keys are preserved. This lets the SPA toggle one surface at
		// a time without sending the whole catalog state back.
		if fc.YcodeShareSurfaces == nil {
			fc.YcodeShareSurfaces = map[string]bool{}
		}
		for k, v := range p.YcodeShareSurfaces {
			fc.YcodeShareSurfaces[k] = v
		}
	}
	if p.Cluster != nil {
		if fc.Cluster == nil {
			fc.Cluster = &conf.ClusterConfig{}
		}
		fc.Cluster.Enabled = *p.Cluster
	}
	if p.ClusterMode != nil {
		// Accept the three canonical modes plus the back-compat aliases
		// ("" / "vkpodman" → vk-podman) and persist the normalized
		// canonical value so on-disk configs converge.
		switch strings.ToLower(strings.TrimSpace(*p.ClusterMode)) {
		case "", "vkpodman", "vk-podman", "agent", "vk-native", "vk-ollama":
			if fc.Cluster == nil {
				fc.Cluster = &conf.ClusterConfig{}
			}
			fc.Cluster.Mode = conf.NormalizeClusterMode(*p.ClusterMode)
		default:
			return BuiltinsResult{}, badRequest("cluster_mode must be one of agent / vk-podman / vk-native / vk-ollama (alias: vkpodman)")
		}
	}
	// UpdateMode is live-read by the upgrade worker on each
	// /admin/upgrade POST, so it doesn't need a restart to take
	// effect. We still save through the same code path because the
	// same FileConfig file owns the value.
	updateModeOnly := p.UpdateMode != nil && p.Shell == nil && p.Desktop == nil && p.Clipboard == nil && p.SSH == nil && p.SSHAllowLocalForward == nil && p.SSHAllowRemoteForward == nil && p.SSHAllowAgentForward == nil && p.SSHForwardSockets == nil && p.SFTP == nil && p.Files == nil && p.FilesAllowWrite == nil && p.FilesScope == nil && p.Podman == nil && p.Sandbox == nil && p.Ollama == nil && p.OllamaPool == nil && p.WarmServing == nil && p.WarmBudgetFrac == nil && p.Otel == nil && p.OtelPool == nil && p.Ycode == nil && p.YcodeShare == nil && p.YcodeShareRequireLogin == nil && p.YcodeShareSurfaces == nil && p.Cluster == nil && p.ClusterMode == nil && p.Mesh == nil && p.MeshPort == nil && p.LANInference == nil && p.LANInferencePort == nil && p.Loom == nil && p.LoomPort == nil && p.BashyServices == nil && p.BashyVersion == nil && p.Zot == nil && p.ZotPort == nil && p.Seaweedfs == nil && p.SeaweedfsPort == nil && p.Kopia == nil && p.KopiaPort == nil && p.Actrunner == nil && p.ActrunnerInstance == nil && p.ActrunnerToken == nil && p.ActrunnerLabels == nil && p.ActrunnerSandbox == nil && p.ActrunnerSandboxImage == nil && p.ActrunnerDockerHost == nil && p.CloudDOEnabled == nil && p.CloudDOToken == nil
	if p.UpdateMode != nil {
		if !conf.ValidUpdateMode(*p.UpdateMode) {
			return BuiltinsResult{}, badRequest("update_mode must be one of auto / manual / never")
		}
		fc.UpdateMode = *p.UpdateMode
		// Clear the legacy bool so it doesn't shadow the new field
		// on the next round-trip through UpdateModeName.
		fc.AutoUpgrade = nil
	}
	if p.AutoRollback != nil {
		fc.AutoRollbackEnabled = p.AutoRollback
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return BuiltinsResult{}, internalErr("%s", err.Error())
	}
	// Persist-then-defer: the new toggle is durable on disk, but the
	// route mount / built-in app registration only happens at boot.
	// Rather than auto-restart (which yanks the admin UI mid-click and
	// breaks batches of operator toggles), advertise RestartPending so
	// the SPA can surface a sticky "Restart to apply" banner and let
	// the operator pull the trigger when their batch is done.
	restart := fc.AgentName != "" && !updateModeOnly
	return BuiltinsResult{OK: true, RestartPending: restart}, nil
}

func upsertBashyService(services *[]conf.BashyService, svc conf.BashyService, enabled *bool, port *int) {
	if services == nil {
		return
	}
	name := strings.TrimSpace(svc.Name)
	if name == "" {
		return
	}
	svc.Name = name
	if svc.AppName == "" {
		svc.AppName = name
	}
	if svc.AppPort == 0 && port != nil {
		svc.AppPort = *port
	}
	if enabled != nil {
		svc.Enabled = *enabled
	}
	for i := range *services {
		if (*services)[i].Name == name {
			existing := (*services)[i]
			existing.Enabled = svc.Enabled
			if svc.AppName != "" {
				existing.AppName = svc.AppName
			}
			if svc.AppPort != 0 {
				existing.AppPort = svc.AppPort
			}
			if svc.MeshService != "" {
				existing.MeshService = svc.MeshService
			}
			if svc.RequireLogin {
				existing.RequireLogin = true
			}
			(*services)[i] = existing
			return
		}
	}
	*services = append(*services, svc)
}

func normalizeBashyServices(in []conf.BashyService) []conf.BashyService {
	out := make([]conf.BashyService, 0, len(in))
	seen := map[string]bool{}
	for _, svc := range in {
		svc.Name = strings.TrimSpace(svc.Name)
		if svc.Name == "" || seen[svc.Name] {
			continue
		}
		seen[svc.Name] = true
		if svc.AppName == "" {
			svc.AppName = svc.Name
		}
		out = append(out, svc)
	}
	return out
}
